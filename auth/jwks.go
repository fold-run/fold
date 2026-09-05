package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// maxJWKSBytes bounds a JWKS response so a hostile or broken issuer cannot
// exhaust gateway memory with an oversized body.
const maxJWKSBytes = 1 << 20 // 1 MiB

// refetchHoldoff is the minimum spacing between fetches of one URI, whatever
// prompted them — an unknown kid (rotation-in, or a flood of forged tokens)
// or a refresh that failed. Without it an IdP outage would cost every
// verification a fetch attempt against the failing endpoint, serialized
// behind the per-URI gate, and the outage would become a latency incident
// for every caller holding a perfectly valid token.
const refetchHoldoff = 30 * time.Second

// jwksCache fetches and caches a JWKS document per URI, single-flighting
// concurrent fetches so an unauthenticated flood of unknown-kid tokens
// cannot be amplified into a burst of outbound requests to the IdP.
//
// A cached set is refreshed once it is older than ttl even when the kid is
// known: rotation-in (a new kid) was always handled by the unknown-kid path,
// but rotation-out — an IdP revoking a key that tokens keep presenting — is
// only visible by re-reading the set, and a key the IdP has withdrawn must
// stop verifying tokens within the ttl rather than for the life of the
// process. A refresh that fails keeps serving the last good set (an IdP
// outage is not a reason to reject every valid caller) and reports it, so
// the outage is visible rather than being read as a wave of bad clients.
type jwksCache struct {
	client *http.Client
	ttl    time.Duration
	now    func() time.Time
	// observe is told the outcome of every fetch, by issuer: "ok", "error"
	// (nothing cached, verification fails), or "stale" (the fetch failed and
	// the previous set was served). The gateway turns this into a metric.
	observe func(issuer, outcome string)

	mu      sync.Mutex
	sets    map[string]*jwksEntry
	fetchMu map[string]*sync.Mutex // per-URI fetch serialization
}

type jwksEntry struct {
	keys    map[string]any // kid → public key
	fetched time.Time      // last successful fetch
	// refreshAt is fetched + ttl: after it, even a known kid forces a
	// refresh. noFetchBefore is the last attempt (success or failure) plus
	// refetchHoldoff: before it, nothing forces one.
	refreshAt     time.Time
	noFetchBefore time.Time
}

func newJWKSCache(client *http.Client) *jwksCache {
	if client == nil {
		// A dedicated client: bounded timeout so a slow IdP cannot pin a
		// verification goroutine, and no ambient default-client sharing.
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &jwksCache{
		client:  client,
		ttl:     5 * time.Minute,
		now:     time.Now,
		observe: func(string, string) {},
		sets:    map[string]*jwksEntry{},
		fetchMu: map[string]*sync.Mutex{},
	}
}

// lookup resolves a key from a set, honoring the single-key-no-kid fallback.
func lookup(keys map[string]any, kid string) (any, bool) {
	if k, ok := keys[kid]; ok {
		return k, true
	}
	// A JWKS with a single key and a token with no kid: accept it. Applied
	// on every path so acceptance never depends on cache age.
	if kid == "" && len(keys) == 1 {
		for _, k := range keys {
			return k, true
		}
	}
	return nil, false
}

// key returns the public key for kid in the set served at uri, fetching the
// set when the kid is unknown or the cached copy has reached its ttl. issuer
// is the configured issuer the URI belongs to, used only to label what is
// reported about the fetch.
func (c *jwksCache) key(ctx context.Context, issuer, uri, kid string) (any, error) {
	if k, ok := c.freshKey(uri, kid); ok {
		return k, nil
	}
	// Serialize fetches per URI: the first miss fetches, concurrent misses
	// wait and then re-read the freshly cached set.
	fm := c.gate(uri)
	fm.Lock()
	defer fm.Unlock()

	// Re-check: another goroutine may have refreshed the set while we waited.
	if k, ok := c.freshKey(uri, kid); ok {
		return k, nil
	}
	c.mu.Lock()
	e := c.sets[uri]
	c.mu.Unlock()
	if e != nil && c.now().Before(e.noFetchBefore) {
		// Fetched — or failed to — within the holdoff. Answer from what is
		// held: a known kid from a set past its ttl is still a key the IdP
		// published, and an unknown one is not worth another fetch yet.
		if k, ok := lookup(e.keys, kid); ok {
			return k, nil
		}
		return nil, fmt.Errorf("jwks %s: no key %q", uri, kid)
	}

	keys, err := c.fetch(ctx, uri)
	now := c.now()
	if err != nil {
		if e == nil {
			c.observe(issuer, "error")
			return nil, err
		}
		// Stale-on-error: the IdP is unreachable and the last set it served
		// is the best evidence of what it trusts. Keep serving it, hold off
		// the next attempt, and say so.
		c.mu.Lock()
		e.noFetchBefore = now.Add(refetchHoldoff)
		c.mu.Unlock()
		c.observe(issuer, "stale")
		if k, ok := lookup(e.keys, kid); ok {
			return k, nil
		}
		return nil, fmt.Errorf("jwks %s: no key %q (refresh failed: %w)", uri, kid, err)
	}
	c.mu.Lock()
	c.sets[uri] = &jwksEntry{
		keys:          keys,
		fetched:       now,
		refreshAt:     now.Add(c.ttl),
		noFetchBefore: now.Add(refetchHoldoff),
	}
	c.mu.Unlock()
	c.observe(issuer, "ok")
	if k, ok := lookup(keys, kid); ok {
		return k, nil
	}
	return nil, fmt.Errorf("jwks %s: no key %q", uri, kid)
}

// freshKey is the hot path: a known kid in a set that has not reached its
// ttl. Anything else goes through the fetch gate.
func (c *jwksCache) freshKey(uri, kid string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.sets[uri]
	if e == nil || !c.now().Before(e.refreshAt) {
		return nil, false
	}
	return lookup(e.keys, kid)
}

func (c *jwksCache) gate(uri string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	fm := c.fetchMu[uri]
	if fm == nil {
		fm = &sync.Mutex{}
		c.fetchMu[uri] = fm
	}
	return fm
}

func (c *jwksCache) fetch(ctx context.Context, uri string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks %s: %w", uri, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch jwks %s: status %d", uri, resp.StatusCode)
	}
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse jwks %s: %w", uri, err)
	}
	keys := map[string]any{}
	for _, raw := range doc.Keys {
		kid, key, err := parseJWK(raw)
		if err != nil {
			continue // skip unsupported key types
		}
		keys[kid] = key
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("jwks %s: no usable keys", uri)
	}
	return keys, nil
}

func parseJWK(raw json.RawMessage) (kid string, key any, err error) {
	var jwk struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Use string `json:"use"`
		N   string `json:"n"`
		E   string `json:"e"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}
	if err := json.Unmarshal(raw, &jwk); err != nil {
		return "", nil, err
	}
	if jwk.Use != "" && jwk.Use != "sig" {
		return "", nil, fmt.Errorf("not a signing key")
	}
	b64 := func(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
	switch jwk.Kty {
	case "RSA":
		n, err := b64(jwk.N)
		if err != nil {
			return "", nil, err
		}
		e, err := b64(jwk.E)
		if err != nil {
			return "", nil, err
		}
		return jwk.Kid, &rsa.PublicKey{
			N: new(big.Int).SetBytes(n),
			E: int(new(big.Int).SetBytes(e).Int64()),
		}, nil
	case "EC":
		var curve elliptic.Curve
		switch jwk.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return "", nil, fmt.Errorf("unsupported curve %q", jwk.Crv)
		}
		x, err := b64(jwk.X)
		if err != nil {
			return "", nil, err
		}
		y, err := b64(jwk.Y)
		if err != nil {
			return "", nil, err
		}
		return jwk.Kid, &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}, nil
	case "OKP":
		if jwk.Crv != "Ed25519" {
			return "", nil, fmt.Errorf("unsupported OKP curve %q", jwk.Crv)
		}
		x, err := b64(jwk.X)
		if err != nil {
			return "", nil, err
		}
		return jwk.Kid, ed25519.PublicKey(x), nil
	}
	return "", nil, fmt.Errorf("unsupported kty %q", jwk.Kty)
}
