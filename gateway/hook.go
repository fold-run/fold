package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/auth"
	"github.com/fold-run/fold/config"
)

// hookWireVersion is the envelope version fold sends. It is part of a public
// interface from the first release — someone writes a hook against it — so it
// changes only when the envelope changes incompatibly, which is a thing this
// project intends never to do.
const hookWireVersion = "1"

// hookMaxBody bounds what fold reads back from a decision endpoint. A hook
// that answers with a gigabyte is a hook that takes the gateway's memory with
// it, and a decision is two short fields.
const hookMaxBody = 64 << 10

// stageIngress inspects the invocation and its arguments, after policy has
// allowed it and before it reaches the upstream.
const stageIngress = "ingress"

// stageEgress inspects the result an upstream returned, before the caller
// sees it and before audit closes the request.
//
// What it can and cannot do is worth stating precisely, because the obvious
// reading is wrong: by the time this runs **the upstream has already acted**.
// A denial here withholds the disclosure, not the effect — the row is deleted,
// the message is sent, and the caller is told the result was refused. Egress
// is a data-loss control. To stop an action, refuse it at ingress.
const stageEgress = "egress"

// hookMaxResultBytes bounds what fold will serialize and send for inspection.
// A result larger than this is not truncated — a partial body is precisely
// the blind spot an inspector must not be handed — so it takes the configured
// error path instead, which means an operator running onError "deny" refuses
// results too large to inspect. That is the fail-safe reading, and it is
// documented rather than discovered.
const hookMaxResultBytes = 1 << 20

// decisionHook is the compiled form of config.Hook: fold's one out-of-process
// seam. It decides, and cannot rewrite — see docs/design-decision-hook.md for
// why that line is what keeps the invisibility rule intact.
type decisionHook struct {
	url       string
	client    *http.Client
	headers   map[string]string
	bearer    string
	failOpen  bool
	stages    map[string]bool
	methods   map[string]bool // empty → every named invocation
	timeout   time.Duration
	principal bool // reserved: always true today, kept for the egress phase
}

// newDecisionHook compiles the config into the form the request path reads.
// Nil config yields nil, and every call site is a nil check — the same shape
// as every other opt-in guard, so a deployment without a hook pays one
// pointer comparison.
func newDecisionHook(cfg *config.Hook) *decisionHook {
	if cfg == nil {
		return nil
	}
	h := &decisionHook{
		url:       cfg.URL,
		headers:   cfg.Headers,
		failOpen:  cfg.OnError == "allow",
		stages:    map[string]bool{},
		methods:   map[string]bool{},
		timeout:   time.Duration(cfg.TimeoutMs) * time.Millisecond,
		principal: true,
	}
	for _, s := range cfg.Stages {
		h.stages[s] = true
	}
	for _, m := range cfg.Methods {
		h.methods[m] = true
	}
	if cfg.BearerSecretRef != "" {
		h.bearer = os.Getenv(cfg.BearerSecretRef)
	}
	// A dedicated client with the same posture as every other outbound
	// trust-path fetch: bounded, and redirects refused — a hook that can
	// redirect fold's decision request elsewhere is a hook that can be
	// repointed by whoever answers it. Keep-alives are the default and are
	// the point: this runs on the proxy path, and a TLS handshake per
	// invocation would dominate the cost the design budgets for.
	h.client = &http.Client{
		Timeout: h.timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("hook: refusing redirect from %q to %q",
				via[0].URL.Redacted(), req.URL.Redacted())
		},
	}
	return h
}

// inspects reports whether this stage and method are configured for
// inspection.
func (h *decisionHook) inspects(stage, method string) bool {
	if h == nil || !h.stages[stage] {
		return false
	}
	return len(h.methods) == 0 || h.methods[method]
}

// hookRequest is the wire envelope. Field names are contract; see the design
// record. It grows only additively, so a hook that ignores unknown fields
// keeps working.
type hookRequest struct {
	Version   string          `json:"version"`
	Stage     string          `json:"stage"`
	Method    string          `json:"method"`
	Name      string          `json:"name,omitempty"`
	Upstream  string          `json:"upstream,omitempty"`
	Principal *hookPrincipal  `json:"principal,omitempty"`
	Tenant    string          `json:"tenant,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// Result is the upstream's answer, verbatim, on the egress stage only.
	Result json.RawMessage `json:"result,omitempty"`
}

type hookPrincipal struct {
	Sub    string   `json:"sub,omitempty"`
	Issuer string   `json:"issuer,omitempty"`
	Groups []string `json:"groups,omitempty"`
}

// hookResponse is what the endpoint answers.
type hookResponse struct {
	Decision string `json:"decision"` // "allow" | "deny"
	Reason   string `json:"reason,omitempty"`
}

// hookOutcome is what happened, for the audit event and the metric. It is
// distinct from the decision: an error that failed open is an allow whose
// provenance an operator needs, because "this call was allowed without
// inspection" is exactly what a compliance review asks about.
type hookOutcome string

const (
	hookAllowed hookOutcome = "allow"
	hookDenied  hookOutcome = "deny"
	hookErrored hookOutcome = "error"
)

// decide asks the hook about one invocation. It returns the outcome, the
// hook's reason when it denied, and never an error — a transport failure is
// resolved into the configured failure decision here, so the call path has
// exactly two answers to handle.
func (h *decisionHook) decide(ctx context.Context, req hookRequest) (hookOutcome, string) {
	body, err := json.Marshal(req)
	if err != nil {
		return h.onError(), ""
	}
	// The hook's own bound, not the caller's: a client that gives up early
	// should not leave fold's decision in flight, but a caller with a
	// generous deadline must not extend the hook past its budget either.
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return h.onError(), ""
	}
	hreq.Header.Set("Content-Type", "application/json")
	for k, v := range h.headers {
		hreq.Header.Set(k, v)
	}
	if h.bearer != "" {
		hreq.Header.Set("Authorization", "Bearer "+h.bearer)
	}

	res, err := h.client.Do(hreq)
	if err != nil {
		return h.onError(), ""
	}
	defer func() {
		// Drain before closing so the connection returns to the pool: this
		// runs on the proxy path, and a fresh connection per decision would
		// dominate the cost the design budgets for.
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, hookMaxBody))
		_ = res.Body.Close()
	}()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return h.onError(), ""
	}
	var out hookResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, hookMaxBody)).Decode(&out); err != nil {
		return h.onError(), ""
	}
	switch out.Decision {
	case "allow":
		return hookAllowed, ""
	case "deny":
		return hookDenied, out.Reason
	default:
		// A hook that answers something other than the two words it was
		// asked for has not decided, so this is an error rather than a
		// generous reading of an unknown string.
		return h.onError(), ""
	}
}

// onError resolves a failed decision into the configured posture. It is the
// only place fold-open-vs-closed is interpreted.
func (h *decisionHook) onError() hookOutcome {
	if h.failOpen {
		return hookErrored
	}
	return hookDenied
}

// hookIngress runs the ingress stage for one invocation, after policy has
// allowed it. It returns a wire error when the call must not proceed.
//
// The event records what happened either way. A denial gets its own outcome
// because an operator reading the trail needs to know whether their policy or
// their inspector refused a call — the remedies live in different systems,
// and often different teams. An error that failed open is recorded on the
// event that proceeded, since a fail-open deployment would otherwise have no
// record that this call went uninspected.
func (g *Gateway) hookIngress(ctx context.Context, evt *audit.Event, rt *routes, u *upstream, method, bare string, args json.RawMessage) error {
	h := rt.hook
	if !h.inspects(stageIngress, method) {
		return nil
	}
	req := hookRequest{
		Version:   hookWireVersion,
		Stage:     stageIngress,
		Method:    method,
		Name:      bare,
		Upstream:  u.cfg.ID,
		Tenant:    evt.Tenant,
		Arguments: args,
	}
	if p := auth.PrincipalFromContext(ctx); p != nil {
		req.Principal = &hookPrincipal{Sub: p.Subject, Issuer: p.Issuer, Groups: p.Groups}
	}

	start := time.Now()
	outcome, reason := h.decide(ctx, req)
	g.metrics.observeHook(stageIngress, string(outcome), time.Since(start))
	evt.HookOutcome = string(outcome)

	switch outcome {
	case hookDenied:
		evt.Outcome = audit.OutcomeHookDenied
		evt.Decision = "deny"
		msg := fmt.Sprintf("decision hook denied %s %q", method, evt.Name)
		if reason != "" {
			// Returned to the caller deliberately: unlike a policy denial,
			// where the expected value is the operator's configuration, a
			// hook denial is usually about the caller's own content, and a
			// refusal nobody can act on becomes a support ticket instead of
			// a fix.
			msg += ": " + reason
		}
		return &jsonrpc.Error{Code: codeDenied, Message: msg}
	case hookErrored:
		// Failed open. The call proceeds uninspected, and says so.
		g.log.Warn("decision hook unavailable; proceeding per onError",
			"stage", stageIngress, "method", method, "upstream", u.cfg.ID)
	}
	return nil
}

// stageServerInitiated inspects what an upstream asks of the caller's client.
// It is the content question design-server-initiated.md declined to answer
// structurally: policy decides whether an upstream may elicit at all, and this
// decides whether *this* elicitation is acceptable — "refuse one that asks for
// a password" is the case that named this stage.
const stageServerInitiated = "serverInitiated"

// hookServerInitiated runs the reverse-path stage against what an upstream is
// asking the caller's client for. A denial refuses the upstream, exactly as a
// policy denial on this path does.
//
// Unlike egress, refusing here prevents the thing: the client is never asked,
// so no model tokens are spent and no human is shown the prompt.
func (g *Gateway) hookServerInitiated(ctx context.Context, evt *audit.Event, u *upstream, method string, params any) error {
	h := g.rt().hook
	if !h.inspects(stageServerInitiated, method) {
		return nil
	}
	req := hookRequest{
		Version:  hookWireVersion,
		Stage:    stageServerInitiated,
		Method:   method,
		Upstream: u.cfg.ID,
		Tenant:   evt.Tenant,
	}
	if p := auth.PrincipalFromContext(ctx); p != nil {
		req.Principal = &hookPrincipal{Sub: p.Subject, Issuer: p.Issuer, Groups: p.Groups}
	}

	start := time.Now()
	var (
		outcome hookOutcome
		reason  string
	)
	// The upstream's own params carry the content worth judging: an
	// elicitation's message and requested schema, a sampling request's
	// messages. They ride in `arguments` rather than a fourth field — this is
	// what the upstream is asking for, which is the same role arguments play
	// on the forward path.
	body, err := json.Marshal(params)
	switch {
	case err != nil || len(body) > hookMaxResultBytes:
		outcome = h.onError()
	default:
		req.Arguments = body
		outcome, reason = h.decide(ctx, req)
	}
	g.metrics.observeHook(stageServerInitiated, string(outcome), time.Since(start))
	evt.HookOutcome = string(outcome)

	switch outcome {
	case hookDenied:
		evt.Outcome = audit.OutcomeHookDenied
		evt.Decision = "deny"
		msg := fmt.Sprintf("decision hook denied %s", method)
		if reason != "" {
			msg += ": " + reason
		}
		return &jsonrpc.Error{Code: codeDenied, Message: msg}
	case hookErrored:
		g.log.Warn("decision hook unavailable; proceeding per onError",
			"stage", stageServerInitiated, "method", method, "upstream", u.cfg.ID)
	}
	return nil
}

// hookEgress runs the egress stage against a result the upstream has already
// produced. It returns a wire error when the result must not be disclosed.
//
// The upstream has acted by now. A denial here is a data-loss control — it
// withholds the answer, not the effect — and the error message says so, so a
// caller who reads "the result was withheld" does not conclude their write
// was rolled back.
func (g *Gateway) hookEgress(ctx context.Context, evt *audit.Event, rt *routes, u *upstream, method, bare string, result any) error {
	h := rt.hook
	if !h.inspects(stageEgress, method) {
		return nil
	}
	req := hookRequest{
		Version:  hookWireVersion,
		Stage:    stageEgress,
		Method:   method,
		Name:     bare,
		Upstream: u.cfg.ID,
		Tenant:   evt.Tenant,
	}
	if p := auth.PrincipalFromContext(ctx); p != nil {
		req.Principal = &hookPrincipal{Sub: p.Subject, Issuer: p.Issuer, Groups: p.Groups}
	}

	start := time.Now()
	var (
		outcome hookOutcome
		reason  string
	)
	// Serializing the result is the cost egress adds over ingress: fold holds
	// the decoded form and the hook needs bytes. It happens only for
	// inspected methods on a configured stage.
	body, err := json.Marshal(result)
	switch {
	case err != nil || len(body) > hookMaxResultBytes:
		outcome = h.onError()
	default:
		req.Result = body
		outcome, reason = h.decide(ctx, req)
	}
	g.metrics.observeHook(stageEgress, string(outcome), time.Since(start))
	// Ingress may have recorded its own outcome; egress is the later word on
	// the same request, and a denial here is what the caller experienced.
	if evt.HookOutcome == "" || outcome != hookAllowed {
		evt.HookOutcome = string(outcome)
	}

	switch outcome {
	case hookDenied:
		evt.Outcome = audit.OutcomeHookDenied
		evt.Decision = "deny"
		msg := fmt.Sprintf("decision hook withheld the result of %s %q (the upstream has already acted)", method, evt.Name)
		if reason != "" {
			msg += ": " + reason
		}
		return &jsonrpc.Error{Code: codeDenied, Message: msg}
	case hookErrored:
		g.log.Warn("decision hook unavailable; disclosing result per onError",
			"stage", stageEgress, "method", method, "upstream", u.cfg.ID)
	}
	return nil
}
