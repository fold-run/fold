package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/fold-run/fold/config"
)

// discoveryDocLimit caps the discovery document size, bounding the memory
// one poll can pin.
const discoveryDocLimit = 4 << 20

// discoverer polls the discovery URL for {"upstreams": [...]} and swaps the
// discovered set into the federation via the reload path. Fail-safe by
// construction: an unreachable source, a malformed document, or one that
// fails merged-config validation (e.g. colliding with a static upstream id)
// leaves the last good set serving.
type discoverer struct {
	g        *Gateway
	cfg      *config.Discovery
	client   *http.Client
	interval time.Duration
	stop     chan struct{}

	lastHash [sha256.Size]byte // last document seen (applied or rejected)
	seen     bool
	failing  bool // dampens repeated fetch-failure logging to transitions

	// statusMu guards the last-sync record surfaced by /api/federation;
	// the metric remains the fleet-wide view.
	statusMu    sync.Mutex
	lastOutcome string // "applied" | "unchanged" | "rejected" | "error"
	lastSyncAt  time.Time
}

// recordSync counts a sync outcome in metrics and remembers it for
// /api/federation.
func (d *discoverer) recordSync(outcome string) {
	d.g.metrics.discoverySync(outcome)
	d.statusMu.Lock()
	d.lastOutcome, d.lastSyncAt = outcome, time.Now()
	d.statusMu.Unlock()
}

// status returns the last sync outcome and time (zero values before the
// first sync completes).
func (d *discoverer) status() (outcome string, at time.Time) {
	d.statusMu.Lock()
	defer d.statusMu.Unlock()
	return d.lastOutcome, d.lastSyncAt
}

func newDiscoverer(g *Gateway, cfg *config.Discovery) *discoverer {
	interval := 30 * time.Second
	if cfg.IntervalMs > 0 {
		interval = time.Duration(cfg.IntervalMs) * time.Millisecond
		// Floor, not rejection: a 1 ms typo turns the gateway into a flood
		// source against its own discovery source, but an aggressive
		// registration should still federate — the same posture as the
		// health-probe clamps.
		if interval < time.Second {
			g.log.Warn("clamping discovery poll interval", "requestedMs", cfg.IntervalMs, "floor", "1s")
			interval = time.Second
		}
	}
	return &discoverer{
		g:   g,
		cfg: cfg,
		client: &http.Client{
			Timeout: 10 * time.Second,
			// The discovery URL is a trust anchor — whoever answers it can
			// add upstreams — and validation forces it onto https. A redirect
			// would let the source hand that authority to a host of its
			// choosing, and Go only strips the bearer credential when the
			// redirect leaves the *domain*, so a sibling host or a plain-http
			// same-host target would still receive it. Neither is ever a
			// legitimate step in fetching a JSON document.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return fmt.Errorf("discovery: refusing redirect from %q to %q",
					via[0].URL.Redacted(), req.URL.Redacted())
			},
		},
		interval: interval,
		stop:     make(chan struct{}),
	}
}

// loop syncs once immediately — a registry that is already populated shapes
// the federation before the first client request lands — then on each
// interval until the gateway closes.
func (d *discoverer) loop() {
	// Each sync runs under rescue (see rescue.go): a poisoned document must
	// cost one sync, not the loop — an ended loop would freeze the federation
	// on the last good set with nothing left polling for the fix.
	d.g.safely("discovery", d.sync)
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			d.g.safely("discovery", d.sync)
		case <-d.stop:
			return
		}
	}
}

func (d *discoverer) sync() {
	doc, err := d.fetch()
	if err != nil {
		d.recordSync("error")
		if !d.failing {
			d.g.log.Warn("discovery fetch failed; keeping current upstream set", "url", d.cfg.URL, "err", err)
			d.failing = true
		}
		return
	}
	if d.failing {
		d.g.log.Info("discovery source recovered", "url", d.cfg.URL)
		d.failing = false
	}

	hash := sha256.Sum256(doc)
	if d.seen && hash == d.lastHash {
		d.recordSync("unchanged")
		return
	}
	// Remember the document — applied or rejected — so a bad document is
	// reported once, not every interval; any fix changes the hash.
	d.lastHash, d.seen = hash, true

	ups, err := parseDiscoveryDoc(doc)
	if err != nil {
		d.recordSync("rejected")
		d.g.log.Warn("discovery document malformed; keeping current upstream set", "url", d.cfg.URL, "err", err)
		return
	}
	if err := d.g.setDiscovered(ups); err != nil {
		d.recordSync("rejected")
		d.g.log.Warn("discovery document rejected; keeping current upstream set", "url", d.cfg.URL, "err", err)
		return
	}
	d.recordSync("applied")
	d.g.log.Info("discovery applied", "upstreams", len(ups))
}

// parseDiscoveryDoc strictly decodes a discovery document — the same
// unknown-field rejection as the main config parser, so a registry typo
// fails loudly instead of silently shipping a half-read upstream.
func parseDiscoveryDoc(data []byte) ([]config.Upstream, error) {
	var parsed struct {
		Upstreams []config.Upstream `json:"upstreams"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&parsed); err != nil {
		return nil, err
	}
	return parsed.Upstreams, nil
}

func (d *discoverer) fetch() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.cfg.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if d.cfg.BearerSecretRef != "" {
		token := os.Getenv(d.cfg.BearerSecretRef)
		if token == "" {
			return nil, fmt.Errorf("bearerSecretRef %s: environment variable is empty", d.cfg.BearerSecretRef)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery source answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, discoveryDocLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > discoveryDocLimit {
		return nil, fmt.Errorf("discovery document exceeds %d bytes", discoveryDocLimit)
	}
	return body, nil
}
