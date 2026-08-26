// Package mcpregistry turns entries in an MCP Registry into fold's discovery
// document. It is a second producer for the format `fold-discovery` already
// writes — the gateway consuming it cannot tell them apart, and does not need
// to.
//
// The registry it reads is the API published by the official MCP Registry
// (https://registry.modelcontextprotocol.io) and by anything speaking the same
// shape, which is the interesting case: the registry is open source, and an
// enterprise running its own has a list of servers it has already approved.
package mcpregistry

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fold-run/fold/config"
)

// DefaultRegistry is the official registry's base URL.
const DefaultRegistry = "https://registry.modelcontextprotocol.io"

// maxBodyBytes bounds a registry response. fold caps every body it reads from
// a remote party; a registry is no more trusted than an upstream.
const maxBodyBytes = 4 << 20

// Server is the subset of a registry entry this producer reads. The registry
// publishes considerably more — packages, repository metadata, publisher
// records — and everything not named here is deliberately ignored rather than
// carried into a document the gateway validates.
type Server struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Title       string   `json:"title"`
	Version     string   `json:"version"`
	Remotes     []Remote `json:"remotes"`
}

// Remote is one endpoint a registry entry advertises.
type Remote struct {
	Type    string         `json:"type"` // "streamable-http" | "sse"
	URL     string         `json:"url"`
	Headers []RemoteHeader `json:"headers"`
}

// RemoteHeader is a header the server declares its callers must send.
type RemoteHeader struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	IsSecret    bool   `json:"isSecret"`
	IsRequired  bool   `json:"isRequired"`
}

// Record is one registry entry as the API returns it: the server document
// plus the registry's own metadata, which lives under a reverse-DNS key
// because the registry versions its metadata separately from the schema.
type Record struct {
	Server Server `json:"server"`
	Meta   struct {
		Official struct {
			Status   string `json:"status"`
			IsLatest bool   `json:"isLatest"`
		} `json:"io.modelcontextprotocol.registry/official"`
	} `json:"_meta"`
}

// Client reads one registry.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Bearer  string // optional: a private registry that authenticates readers
}

// NewClient builds a client against base. The HTTP client refuses redirects
// outright: the base URL decides which registry is authoritative, and a
// redirect would let that registry hand the decision — and any configured
// bearer — to a host the operator never named. Same rule fold's discovery
// poller and audit webhook already follow.
func NewClient(base, bearer string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("registry url must be an absolute http(s) URL, got %q", base)
	}
	return &Client{
		BaseURL: strings.TrimSuffix(base, "/"),
		Bearer:  bearer,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return fmt.Errorf("mcpregistry: refusing redirect from %q to %q",
					via[0].URL.Redacted(), req.URL.Redacted())
			},
		},
	}, nil
}

// errNotFound distinguishes "the registry does not have this server" from a
// transport failure, because the two mean different things to an operator: a
// typo in the allowlist, or a registry that is down.
var errNotFound = errors.New("not found in registry")

// Latest fetches the latest published version of one server by name.
//
// One request per allowlisted name rather than paging the whole registry: the
// allowlist is curated and small, the endpoint answers the exact question, and
// paging tens of thousands of public entries to find four of them would make
// the sync cost scale with someone else's registry rather than with this
// deployment.
func (c *Client) Latest(ctx context.Context, name string) (*Record, error) {
	// The name is reverse-DNS with a slash — it is one path segment, so the
	// slash must be escaped or the registry reads it as two.
	target := c.BaseURL + "/v0.1/servers/" + url.PathEscape(name) + "/versions/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	var e Record
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, fmt.Errorf("registry response is not a server document: %w", err)
	}
	return &e, nil
}

// Entry is one line of the operator's allowlist: a registry name, and the
// identity fold should give it.
type Entry struct {
	// Name is the registry's own reverse-DNS name, e.g.
	// "io.github.github/github-mcp-server".
	Name string `json:"name"`
	// ID overrides the derived upstream id. Optional.
	ID string `json:"id,omitempty"`
	// Namespace overrides the derived MCP namespace. Optional, and usually
	// worth setting: the derived form is the whole registry name flattened,
	// which is unambiguous and long, and the namespace is what prefixes
	// every tool name a model reads.
	Namespace string `json:"namespace,omitempty"`
}

// Allowlist is the document the producer is pointed at.
type Allowlist struct {
	Servers []Entry `json:"servers"`
}

// ParseAllowlist reads and checks an allowlist document. Unknown fields are
// rejected: a misspelled key in a file whose whole job is to bound what gets
// federated should fail loudly rather than widen it silently.
func ParseAllowlist(data []byte) (*Allowlist, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var a Allowlist
	if err := dec.Decode(&a); err != nil {
		return nil, err
	}
	if len(a.Servers) == 0 {
		return nil, errors.New("allowlist names no servers")
	}
	seen := map[string]bool{}
	for i, e := range a.Servers {
		if strings.TrimSpace(e.Name) == "" {
			return nil, fmt.Errorf("servers[%d]: name is required", i)
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("servers[%d]: %q is listed twice", i, e.Name)
		}
		seen[e.Name] = true
	}
	return &a, nil
}

var nonIdent = regexp.MustCompile(`[^a-z0-9]+`)

// Identifier flattens a registry name into something fold accepts as an id or
// namespace (`^[a-z0-9][a-z0-9-]*$`). It is deterministic and collision-safe
// across publishers because it keeps the whole reverse-DNS name — two
// publishers with the same short name do not collapse onto one identity, which
// matters more here than brevity: the operator can always override it.
func Identifier(name string) string {
	s := nonIdent.ReplaceAllString(strings.ToLower(name), "-")
	return strings.Trim(s, "-")
}

// MapOptions bounds what a registry entry may become.
type MapOptions struct {
	// ReservedIDs are ids and namespaces a registry entry may not claim —
	// list the gateway's static upstream ids, so a registry entry cannot
	// publish a collision that makes the gateway reject the whole document
	// and freeze discovery on the last good set.
	ReservedIDs []string
	// AllowSecretHeaders federates an entry whose remote declares a header
	// it will not work without. Off by default: this producer never emits a
	// credential, so such an upstream would join the federation and fail
	// every call. See mapEntry for the whole argument.
	AllowSecretHeaders bool
}

// Map converts fetched entries into upstream entries, in allowlist order,
// skipping what it cannot federate with a log line saying why. One unusable
// entry never takes the rest of the document down — the same posture the
// Kubernetes producer takes toward one bad Service.
//
// The result is sorted by id, so the rendered document is byte-stable and the
// consuming gateway's change detection does not fire on map ordering.
func Map(entries map[string]*Record, list *Allowlist, opts MapOptions, log *slog.Logger) []config.Upstream {
	reserved := map[string]bool{}
	for _, r := range opts.ReservedIDs {
		reserved[r] = true
	}
	seenID := map[string]int{}
	seenNS := map[string]int{}
	type candidate struct {
		u    config.Upstream
		name string
	}
	var candidates []candidate
	for _, want := range list.Servers {
		e, ok := entries[want.Name]
		if !ok || e == nil {
			continue // fetch failed or absent; already logged by the caller
		}
		u, err := mapEntry(e, want, opts)
		if err != nil {
			log.Warn("skipping registry server", "server", want.Name, "err", err)
			continue
		}
		single := config.Config{Upstreams: []config.Upstream{u}}
		if err := single.Validate(); err != nil {
			log.Warn("skipping registry server", "server", want.Name, "err", err)
			continue
		}
		if reserved[u.ID] || reserved[u.Namespace] {
			log.Warn("skipping registry server: id or namespace is reserved",
				"server", want.Name, "id", u.ID, "namespace", u.Namespace)
			continue
		}
		seenID[u.ID]++
		seenNS[u.Namespace]++
		candidates = append(candidates, candidate{u: u, name: want.Name})
	}
	// Contested identities fail closed, dropping every claimant. Unlike the
	// Kubernetes producer this cannot be an attack — the operator wrote the
	// allowlist — but it is a mistake worth refusing rather than resolving
	// arbitrarily, because whichever entry won would depend on file order.
	var ups []config.Upstream
	for _, c := range candidates {
		if seenID[c.u.ID] > 1 || seenNS[c.u.Namespace] > 1 {
			log.Warn("skipping registry server: id or namespace claimed more than once (all claimants dropped)",
				"server", c.name, "id", c.u.ID, "namespace", c.u.Namespace)
			continue
		}
		ups = append(ups, c.u)
	}
	slices.SortFunc(ups, func(a, b config.Upstream) int { return strings.Compare(a.ID, b.ID) })
	return ups
}

func mapEntry(e *Record, want Entry, opts MapOptions) (config.Upstream, error) {
	official := e.Meta.Official
	// A registry entry that has been deleted or deprecated upstream stops
	// being federated, which is most of the point of tracking a registry
	// rather than copying a URL into config once.
	if official.Status != "" && official.Status != "active" {
		return config.Upstream{}, fmt.Errorf("registry status is %q", official.Status)
	}

	var remote *Remote
	var dropped int
	for i := range e.Server.Remotes {
		r := &e.Server.Remotes[i]
		// fold speaks streamable HTTP. The deprecated HTTP+SSE transport is
		// not implemented and is on the specification's removal clock, so an
		// sse-only entry is not federatable rather than not yet supported.
		if r.Type != "streamable-http" {
			continue
		}
		if remote == nil {
			remote = r
			continue
		}
		dropped++
	}
	if remote == nil {
		// The common cause is an entry that ships only packages — an npm or
		// OCI stdio server, which has no network endpoint to federate. That
		// is what fold-stdio exists for, and it is a deliberate operator act
		// rather than something a registry sync should conjure.
		return config.Upstream{}, errors.New("no streamable-http remote (package-only entries need fold-stdio)")
	}
	if dropped > 0 {
		// Multiple streamable-http remotes are not documented as
		// interchangeable, and fold's multi-endpoint `urls` means "the same
		// server, reachable several ways" — with sessions pinned per
		// endpoint. Assuming that of two URLs a registry lists side by side
		// would be a guess with a correctness cost, so take the first.
		return config.Upstream{}, fmt.Errorf(
			"entry advertises %d streamable-http remotes; ambiguous which to federate", dropped+1)
	}

	if !opts.AllowSecretHeaders {
		for _, h := range remote.Headers {
			if h.IsSecret {
				// This producer never emits credentials, by design: it reads
				// a document it does not control, and a producer that could
				// name secrets would be handing the registry the ability to
				// point gateway-held secrets at endpoints of its choosing —
				// the exfiltration path discovery's allowlists exist to
				// close. An entry needing one belongs in the base config,
				// where a human chose the secret and the destination
				// together.
				return config.Upstream{}, fmt.Errorf(
					"remote requires a secret header %q; configure it as a static upstream", h.Name)
			}
		}
	}

	id := want.ID
	if id == "" {
		id = Identifier(e.Server.Name)
	}
	ns := want.Namespace
	if ns == "" {
		ns = id
	}
	u := config.Upstream{
		ID:        id,
		Namespace: ns,
		URL:       remote.URL,
	}
	// The registry's own description is the closest thing to an owner record
	// it publishes, and fold's Owner carries contact metadata into audit and
	// health. Only the registry name goes in: anything else would be this
	// producer inventing provenance.
	u.Owner = &config.Owner{Org: e.Server.Name}
	return u, nil
}

// Document renders the discovery document.
func Document(ups []config.Upstream) ([]byte, error) {
	if ups == nil {
		ups = []config.Upstream{}
	}
	return json.Marshal(map[string]any{"upstreams": ups})
}

// Producer runs the fetch→map→serve loop and serves the document for a
// gateway's discovery.url to poll.
type Producer struct {
	Client    *Client
	Allowlist *Allowlist
	Interval  time.Duration // default 5m
	Bearer    string        // non-empty → required Authorization: Bearer value
	Map       MapOptions
	Log       *slog.Logger

	mu        sync.RWMutex
	doc       []byte
	upstreams int
	lastSync  time.Time
	lastErr   error
}

// Run syncs immediately, then on the interval, until ctx ends.
func (p *Producer) Run(ctx context.Context) {
	interval := p.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	p.sync(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.sync(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// sync fetches every allowlisted server, maps, and publishes.
//
// A failed fetch drops that one server from this round rather than failing the
// sync: a registry that 500s on one entry should not empty a federation. But
// if *every* fetch fails the document is left alone, because that is a
// registry outage rather than an emptied allowlist, and publishing an empty
// document would tell the gateway to retire every discovered upstream.
func (p *Producer) sync(ctx context.Context) {
	fetched := map[string]*Record{}
	var failures int
	for _, want := range p.Allowlist.Servers {
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		e, err := p.Client.Latest(fctx, want.Name)
		cancel()
		if err != nil {
			failures++
			if errors.Is(err, errNotFound) {
				p.Log.Warn("registry server not found", "server", want.Name)
			} else {
				p.Log.Warn("registry fetch failed", "server", want.Name, "err", err)
			}
			continue
		}
		fetched[want.Name] = e
	}
	if failures == len(p.Allowlist.Servers) {
		err := fmt.Errorf("all %d registry fetches failed", failures)
		p.mu.Lock()
		p.lastErr = err
		p.mu.Unlock()
		p.Log.Warn("keeping last document", "err", err)
		return
	}

	ups := Map(fetched, p.Allowlist, p.Map, p.Log)
	doc, err := Document(ups)
	if err != nil {
		p.mu.Lock()
		p.lastErr = err
		p.mu.Unlock()
		p.Log.Warn("document render failed; keeping last document", "err", err)
		return
	}
	p.mu.Lock()
	changed := string(doc) != string(p.doc)
	p.doc, p.upstreams, p.lastSync = doc, len(ups), time.Now()
	p.lastErr = nil
	p.mu.Unlock()
	if changed {
		p.Log.Info("discovery document updated", "servers", len(fetched), "upstreams", len(ups))
	}
}

// Handler serves the document and a health summary, with the same shape and
// the same posture as the Kubernetes producer's: 503 before the first
// successful sync, because serving an empty document then would wipe the
// consuming gateway's discovered set.
func (p *Producer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.RLock()
		defer p.mu.RUnlock()
		status := http.StatusOK
		if p.doc == nil {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		errText := ""
		if p.lastErr != nil {
			// A registry error can name the configured base URL; a
			// deployment that gates the document gets a category, not the
			// text, mirroring the gateway's /health posture.
			if p.Bearer != "" {
				errText = "sync failing"
			} else {
				errText = p.lastErr.Error()
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"synced":    !p.lastSync.IsZero(),
			"lastSync":  p.lastSync,
			"upstreams": p.upstreams,
			"error":     errText,
		})
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if p.Bearer != "" {
			presented := []byte(r.Header.Get("Authorization"))
			expected := []byte("Bearer " + p.Bearer)
			if subtle.ConstantTimeCompare(presented, expected) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		p.mu.RLock()
		doc := p.doc
		p.mu.RUnlock()
		if doc == nil {
			http.Error(w, "no successful sync yet", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	})
	return mux
}
