// Package kubediscovery produces fold's discovery document from Kubernetes
// Services: list Services matching a label selector on an interval, map
// them to upstream entries via annotations, validate with fold's own config
// package, and serve {"upstreams": [...]} for the gateway's discovery.url
// to poll.
//
// It is deliberately poll-to-poll: fold polls the producer, the producer
// polls the Kubernetes list API. No informers, no client-go — the pod's
// service-account token and plain HTTP are the whole client, which keeps
// fold's module free of the Kubernetes dependency tree.
package kubediscovery

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fold-run/fold/config"
)

// Annotations on a labeled Service. Every field of a fold upstream is
// reachable through fold.run/config; the rest are conveniences for the
// common case.
const (
	// AnnID overrides the upstream id (default: the Service name).
	AnnID = "fold.run/id"
	// AnnNamespace overrides the MCP namespace (default: the upstream id).
	AnnNamespace = "fold.run/namespace"
	// AnnPort selects the Service port (default: the port named "mcp",
	// else the first port).
	AnnPort = "fold.run/port"
	// AnnPath overrides the MCP path (default "/mcp").
	AnnPath = "fold.run/path"
	// AnnScheme overrides the URL scheme (default "http" — in-cluster).
	AnnScheme = "fold.run/scheme"
	// AnnURL replaces the derived cluster-DNS URL entirely (external or
	// non-standard endpoints).
	AnnURL = "fold.run/url"
	// AnnConfig is a JSON object decoded as the upstream entry itself —
	// auth, rate limits, timeouts, anything fold's schema allows. Derived
	// defaults fill only fields the JSON leaves unset.
	AnnConfig = "fold.run/config"
)

// DefaultSelector is the label selector for Services to federate.
const DefaultSelector = "fold.run/upstream=true"

// listBodyLimit caps a ServiceList response.
const listBodyLimit = 32 << 20

// Service is the minimal shape read from the Kubernetes API.
type Service struct {
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Ports []struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		} `json:"ports"`
	} `json:"spec"`
}

// Client lists Services from the Kubernetes API.
type Client struct {
	apiURL    string
	tokenFile string // re-read per request: bound tokens rotate
	http      *http.Client
}

// InClusterDefaults resolves the API URL, token file, and CA file from the
// standard in-cluster environment.
func InClusterDefaults() (apiURL, tokenFile, caFile string, err error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return "", "", "", fmt.Errorf("not in cluster: KUBERNETES_SERVICE_HOST/PORT unset (use --kube-api)")
	}
	const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	return "https://" + host + ":" + port, saDir + "/token", saDir + "/ca.crt", nil
}

// NewClient builds a list client. tokenFile and caFile may be empty (no
// auth header, system CA pool) — useful against test servers and proxies.
func NewClient(apiURL, tokenFile, caFile string) (*Client, error) {
	parsed, err := url.Parse(apiURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("kube api url %q must be absolute http(s)", apiURL)
	}
	transport := http.DefaultTransport
	if caFile != "" {
		pem, err := os.ReadFile(caFile) //nolint:gosec // operator-supplied CA path
		if err != nil {
			return nil, fmt.Errorf("read ca file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca file %q contains no certificates", caFile)
		}
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		transport = t
	}
	return &Client{
		apiURL:    strings.TrimSuffix(apiURL, "/"),
		tokenFile: tokenFile,
		http:      &http.Client{Transport: transport, Timeout: 15 * time.Second},
	}, nil
}

// ListServices lists Services matching selector, cluster-wide when
// namespace is empty.
func (c *Client) ListServices(ctx context.Context, namespace, selector string) ([]Service, error) {
	path := "/api/v1/services"
	if namespace != "" {
		path = "/api/v1/namespaces/" + url.PathEscape(namespace) + "/services"
	}
	u := c.apiURL + path + "?labelSelector=" + url.QueryEscape(selector)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.tokenFile != "" {
		token, err := os.ReadFile(c.tokenFile) //nolint:gosec // operator-supplied token path
		if err != nil {
			return nil, fmt.Errorf("read service account token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kubernetes api answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, listBodyLimit))
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []Service `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decode service list: %w", err)
	}
	return list.Items, nil
}

// validationAuth lets per-entry validation accept caller-derived credential
// strategies (passthrough, token-exchange), which fold only permits with
// auth required — a property of the consuming gateway the producer cannot
// know. Everything else about the entry is validated for real.
var validationAuth = &config.Auth{
	Mode:     "required",
	Resource: "https://validate.invalid",
	Issuers:  []config.Issuer{{Issuer: "https://validate.invalid"}},
}

// MapOptions bounds what labeled Services may register. The zero value is
// the restrictive default: no credentialed auth strategies and no secretRef
// names are permitted — Service authors can add plumbing, not point
// gateway-held secrets at endpoints of their choosing — and nothing is
// reserved or prefixed.
type MapOptions struct {
	// AllowedAuthStrategies lists the credential strategies a Service's
	// fold.run/config may carry ("none" needs no listing). Empty → none.
	AllowedAuthStrategies []string
	// AllowedSecretRefs lists the environment variable names a Service may
	// reference in secretRef fields. Empty → none.
	AllowedSecretRefs []string
	// ReservedIDs are upstream ids and MCP namespaces Services may not
	// claim — list the gateway's static upstreams here so a labeled Service
	// cannot publish a collision that makes the gateway reject the whole
	// document (freezing discovery on the last good set).
	ReservedIDs []string
	// UnprefixedIDs disables namespace prefixing. Prefixing is the default
	// because without it any team that can label a Service can claim
	// another team's id or MCP namespace — first-wins by API list order, so
	// an attacker picks an earlier-sorting namespace and both hijacks the
	// routing identity and evicts the legitimate owner. Only disable it in
	// single-tenant clusters.
	UnprefixedIDs bool

	// AllowedCredentialHosts bounds where a credentialed Service may send
	// secrets (its endpoint hosts and tokenEndpoint host). Empty with any
	// credential allowed means unrestricted — set it whenever
	// AllowedSecretRefs is set, since naming a secret and choosing its
	// destination are equally load-bearing.
	AllowedCredentialHosts []string

	// MinHealthCheckIntervalMs floors a Service's healthCheck.intervalMs
	// (default 1000): a Service must not be able to turn the gateway into a
	// probe flood.
	MinHealthCheckIntervalMs int
}

// MapServices converts labeled Services into upstream entries under opts.
// Invalid, disallowed, reserved, and colliding entries are skipped with a
// log line — one bad Service must never take the rest of the federation's
// document down. The result is sorted by id so the document is byte-stable
// for fold's change detection.
func MapServices(services []Service, opts MapOptions, log *slog.Logger) []config.Upstream {
	seenID := map[string]int{}
	seenNS := map[string]int{}
	reserved := map[string]bool{}
	for _, r := range opts.ReservedIDs {
		reserved[r] = true
	}
	type candidate struct {
		u       config.Upstream
		service string
	}
	var candidates []candidate
	for _, svc := range services {
		name := svc.Metadata.Namespace + "/" + svc.Metadata.Name
		u, err := mapService(svc, opts)
		if err != nil {
			log.Warn("skipping service", "service", name, "err", err)
			continue
		}
		if err := checkCredentialDestinations(&u, opts); err != nil {
			log.Warn("skipping service", "service", name, "err", err)
			continue
		}
		single := config.Config{Upstreams: []config.Upstream{u}, Auth: validationAuth}
		if err := single.Validate(); err != nil {
			log.Warn("skipping service", "service", name, "err", err)
			continue
		}
		if reserved[u.ID] || reserved[u.Namespace] {
			log.Warn("skipping service: id or namespace is reserved",
				"service", name, "id", u.ID, "namespace", u.Namespace)
			continue
		}
		seenID[u.ID]++
		seenNS[u.Namespace]++
		candidates = append(candidates, candidate{u: u, service: name})
	}
	// Contested identities fail closed: dropping *every* claimant beats
	// first-wins, which would hand the identity to whichever Service the
	// API happened to list first — an ordering an attacker chooses by
	// picking an earlier-sorting namespace, hijacking the identity and
	// evicting its legitimate owner in one move.
	var ups []config.Upstream
	for _, c := range candidates {
		if seenID[c.u.ID] > 1 || seenNS[c.u.Namespace] > 1 {
			log.Warn("skipping service: id or namespace is claimed by more than one service (all claimants dropped)",
				"service", c.service, "id", c.u.ID, "namespace", c.u.Namespace)
			continue
		}
		ups = append(ups, c.u)
	}
	slices.SortFunc(ups, func(a, b config.Upstream) int { return strings.Compare(a.ID, b.ID) })
	return ups
}

func mapService(svc Service, opts MapOptions) (config.Upstream, error) {
	ann := svc.Metadata.Annotations
	var u config.Upstream
	if raw := ann[AnnConfig]; raw != "" {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&u); err != nil {
			return u, fmt.Errorf("%s: %w", AnnConfig, err)
		}
	}
	if err := checkAuthAllowed(u.Auth, opts); err != nil {
		return u, err
	}
	if u.ID == "" {
		if u.ID = ann[AnnID]; u.ID == "" {
			u.ID = svc.Metadata.Name
			if !opts.UnprefixedIDs {
				u.ID = svc.Metadata.Namespace + "-" + svc.Metadata.Name
			}
		}
	}
	if u.Namespace == "" {
		if u.Namespace = ann[AnnNamespace]; u.Namespace == "" {
			u.Namespace = u.ID
		}
	}
	if !opts.UnprefixedIDs {
		// Both identities must carry the prefix. The MCP namespace is the
		// one clients actually route on ({namespace}__{tool}), so
		// constraining the id alone would still let a team claim another's
		// tool namespace — with the winner decided by API list order.
		prefix := svc.Metadata.Namespace + "-"
		for _, claim := range []struct{ what, value string }{{"id", u.ID}, {"namespace", u.Namespace}} {
			if !strings.HasPrefix(claim.value, prefix) {
				return u, fmt.Errorf("%s %q must carry the %q prefix (namespace prefixing; --allow-unprefixed-ids to disable)", claim.what, claim.value, prefix)
			}
		}
	}
	// Attribution must not be self-asserted: owner surfaces in /healthz and
	// audit, so a Service could otherwise claim to be another team's.
	u.Owner = &config.Owner{Org: svc.Metadata.Namespace, Team: svc.Metadata.Name}
	if hc := u.HealthCheck; hc != nil {
		floor := opts.MinHealthCheckIntervalMs
		if floor <= 0 {
			floor = 1000
		}
		if hc.IntervalMs < floor {
			clamped := *hc
			clamped.IntervalMs = floor
			u.HealthCheck = &clamped
		}
	}
	if u.URL == "" && len(u.URLs) == 0 {
		if override := ann[AnnURL]; override != "" {
			u.URL = override
		} else {
			port, err := servicePort(svc)
			if err != nil {
				return u, err
			}
			scheme := ann[AnnScheme]
			if scheme == "" {
				scheme = "http"
			}
			path := ann[AnnPath]
			if path == "" {
				path = "/mcp"
			}
			u.URL = fmt.Sprintf("%s://%s.%s.svc.cluster.local:%d%s",
				scheme, svc.Metadata.Name, svc.Metadata.Namespace, port, path)
		}
	}
	return u, nil
}

// checkAuthAllowed applies the credential allowlists: a Service may only
// carry auth strategies and secretRef names the operator granted. The
// destination URL is the Service author's to choose, so an ungated
// credential reference would let any author with labeling rights point a
// gateway-held secret at their own endpoint.
func checkAuthAllowed(a *config.UpstreamAuth, opts MapOptions) error {
	if a == nil {
		return nil
	}
	if a.Strategy != "" && a.Strategy != "none" && !slices.Contains(opts.AllowedAuthStrategies, a.Strategy) {
		return fmt.Errorf("auth strategy %q is not allowed (--allow-auth-strategies)", a.Strategy)
	}
	refs := []string{a.SecretRef}
	if a.ClientAuth != nil {
		refs = append(refs, a.ClientAuth.SecretRef)
	}
	for _, ref := range refs {
		if ref != "" && !slices.Contains(opts.AllowedSecretRefs, ref) {
			return fmt.Errorf("secretRef %q is not allowed (--allow-secret-refs)", ref)
		}
	}
	// The token endpoint receives the client secret (and, under
	// token-exchange, the caller's token) — gate it like any other
	// credential destination.
	if a.TokenEndpoint != "" && opts.AllowedCredentialHosts != nil {
		parsed, err := url.Parse(a.TokenEndpoint)
		if err != nil || !config.HostAllowed(opts.AllowedCredentialHosts, parsed.Host) {
			return fmt.Errorf("tokenEndpoint %q is not an allowed credential host (--allow-credential-hosts)", a.TokenEndpoint)
		}
	}
	return nil
}

// checkCredentialDestinations bounds where a credentialed upstream may send
// secrets: naming an allowed secret is half the exposure, the destination
// is the other half, and a Service author chooses both.
func checkCredentialDestinations(u *config.Upstream, opts MapOptions) error {
	if u.Auth == nil || u.Auth.Strategy == "" || u.Auth.Strategy == "none" || opts.AllowedCredentialHosts == nil {
		return nil
	}
	for _, raw := range u.Endpoints() {
		parsed, err := url.Parse(raw)
		if err != nil || !config.HostAllowed(opts.AllowedCredentialHosts, parsed.Host) {
			return fmt.Errorf("credentialed endpoint %q is not an allowed credential host (--allow-credential-hosts)", raw)
		}
	}
	return nil
}

// servicePort picks the annotation-named port, else the port named "mcp",
// else the first port.
func servicePort(svc Service) (int, error) {
	if raw := svc.Metadata.Annotations[AnnPort]; raw != "" {
		var port int
		if _, err := fmt.Sscanf(raw, "%d", &port); err != nil || port <= 0 || port > 65535 {
			return 0, fmt.Errorf("%s: %q is not a port", AnnPort, raw)
		}
		return port, nil
	}
	if len(svc.Spec.Ports) == 0 {
		return 0, fmt.Errorf("service has no ports and no %s annotation", AnnURL)
	}
	for _, p := range svc.Spec.Ports {
		if p.Name == "mcp" {
			return p.Port, nil
		}
	}
	return svc.Spec.Ports[0].Port, nil
}

// Document renders the discovery document fold polls.
func Document(ups []config.Upstream) ([]byte, error) {
	if ups == nil {
		ups = []config.Upstream{}
	}
	return json.Marshal(map[string]any{"upstreams": ups})
}

// Producer runs the list→map→serve loop.
type Producer struct {
	Client    *Client
	Namespace string        // "" = cluster-wide
	Selector  string        // label selector (DefaultSelector by default)
	Interval  time.Duration // default 15s
	Bearer    string        // non-empty → required Authorization: Bearer value
	Map       MapOptions    // registration bounds (zero value: restrictive)
	Log       *slog.Logger

	mu        sync.RWMutex
	doc       []byte
	upstreams int
	lastSync  time.Time
	lastErr   error
}

func (p *Producer) selector() string {
	if p.Selector != "" {
		return p.Selector
	}
	return DefaultSelector
}

// Run syncs immediately, then on the interval, until ctx ends.
func (p *Producer) Run(ctx context.Context) {
	interval := p.Interval
	if interval <= 0 {
		interval = 15 * time.Second
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

// sync lists, maps, and publishes. A list failure keeps the last good
// document serving — the same fail-safe posture as fold's consumer side.
func (p *Producer) sync(ctx context.Context) {
	lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	services, err := p.Client.ListServices(lctx, p.Namespace, p.selector())
	if err != nil {
		p.mu.Lock()
		p.lastErr = err
		p.mu.Unlock()
		p.Log.Warn("service list failed; keeping last document", "err", err)
		return
	}
	ups := MapServices(services, p.Map, p.Log)
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
	p.doc, p.upstreams, p.lastSync, p.lastErr = doc, len(ups), time.Now(), nil
	p.mu.Unlock()
	if changed {
		p.Log.Info("discovery document updated", "services", len(services), "upstreams", len(ups))
	}
}

// Handler serves the document (any GET path except /healthz) and a health
// summary. Before the first successful sync it answers 503 — serving an
// empty document then would wipe the consuming gateway's discovered set.
func (p *Producer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.RLock()
		defer p.mu.RUnlock()
		status := http.StatusOK
		if p.doc == nil {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		// Health is unauthenticated. Raw kube-API errors can name internal
		// hosts and token paths, so a deployment that gates the document
		// with a bearer token gets a category here, not the text —
		// mirroring the gateway's own /healthz posture.
		errText := ""
		if p.lastErr != nil {
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
