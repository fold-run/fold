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
	"math/big"
	"net/http"
	"sync"
	"time"
)

// jwksCache fetches and caches a JWKS document per URI.
type jwksCache struct {
	client *http.Client
	ttl    time.Duration

	mu   sync.Mutex
	sets map[string]*jwksEntry
}

type jwksEntry struct {
	keys    map[string]any // kid → public key
	fetched time.Time
}

func newJWKSCache(client *http.Client) *jwksCache {
	if client == nil {
		client = http.DefaultClient
	}
	return &jwksCache{client: client, ttl: 5 * time.Minute, sets: map[string]*jwksEntry{}}
}

// key returns the public key for kid at uri, refetching the set when the kid
// is unknown and the cached copy is stale (key rotation).
func (c *jwksCache) key(ctx context.Context, uri, kid string) (any, error) {
	c.mu.Lock()
	e := c.sets[uri]
	if e != nil {
		if k, ok := e.keys[kid]; ok {
			c.mu.Unlock()
			return k, nil
		}
		if time.Since(e.fetched) < 30*time.Second {
			c.mu.Unlock()
			return nil, fmt.Errorf("jwks %s: no key %q", uri, kid)
		}
	}
	c.mu.Unlock()

	keys, err := c.fetch(ctx, uri)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.sets[uri] = &jwksEntry{keys: keys, fetched: time.Now()}
	c.mu.Unlock()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	// A JWKS with a single key and no kid on the token: accept it.
	if kid == "" && len(keys) == 1 {
		for _, k := range keys {
			return k, nil
		}
	}
	return nil, fmt.Errorf("jwks %s: no key %q", uri, kid)
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
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
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
