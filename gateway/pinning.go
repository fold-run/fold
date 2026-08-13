package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/internal/state"
)

// pinTTL bounds how long a baseline survives without being seen. It is long
// enough that a tool nobody lists for a month is genuinely unfamiliar when it
// reappears, and short enough that a retired upstream's records do not
// accumulate forever.
const pinTTL = 30 * 24 * time.Hour

// definitionPins remembers the digest of each definition an upstream has
// advertised, so a rewritten one is a fact rather than an assumption.
//
// The baseline lives in state.Store rather than in memory for a reason that is
// about the attack rather than about tidiness: a per-instance baseline means
// each pod trusts whatever it saw first, so a rolling restart re-pins the whole
// federation to whatever is current — precisely the moment worth choosing if
// you are the one rewriting a definition. Shared state makes the fleet agree
// on what "unchanged" means, and a Redis outage degrades to the local mirror
// the way task ownership already does.
type definitionPins struct {
	store state.Store
	// report is called for each changed definition; nil disables the whole
	// mechanism, which is what "off" compiles to.
	report func(kind, name, was, now string)
}

// digestDefinition hashes a definition as a model and a policy see it.
//
// The upstream's own form is hashed, before namespacing: a rewrite fold
// performs is fold's, and must not read as the upstream changing its mind.
// Annotations are in the digest deliberately — a tool that flips
// destructiveHint has edited its authorization rather than its documentation,
// and once policy gates on annotations that is an escalation performed by the
// party being gated.
func digestDefinition(v any) string {
	// json.Marshal of the SDK's structs is stable: fields are emitted in
	// declaration order, and the schemas are json.RawMessage carried
	// verbatim. Any encoding change would show up as one drift event per
	// definition on upgrade, which is noisy but not wrong — and the test
	// pins today's behaviour so the change is deliberate rather than noticed
	// in production.
	data, err := json.Marshal(v)
	if err != nil {
		// A definition that cannot be marshalled cannot be compared; treat it
		// as unpinnable rather than failing the list it arrived in.
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// pinned is the record kept per definition. Only the digest is compared; the
// timestamp is for humans reading the store.
type pinned struct {
	Digest    string `json:"digest"`
	FirstSeen int64  `json:"firstSeen"`
}

// check compares one list's worth of definitions against their baselines and
// adopts what it finds. Adoption is the point: a change is reported once, and
// the new definition becomes what the next refill is measured against. An
// alert that repeated on every refill would be filtered within a day, and a
// filtered alert is the same as no alert.
//
// It runs on the list-refill path — once per cache generation, not once per
// request — which is what makes it affordable at all.
func (p *definitionPins) check(ctx context.Context, kind string, names []string, digests []string) {
	if p == nil || p.report == nil || len(names) == 0 {
		return
	}
	keys := make([]string, 0, len(names))
	for _, name := range names {
		keys = append(keys, pinKey(kind, name))
	}
	found := p.store.GetMany(ctx, keys)
	adopt := make(map[string][]byte, len(names))
	now := time.Now().Unix()
	for i, name := range names {
		digest := digests[i]
		if digest == "" {
			continue
		}
		key := keys[i]
		raw, ok := found[key]
		if !ok {
			// First sight. Trust-on-first-use cannot vouch for this
			// definition — it can only notice the next one differing — so
			// this is recorded silently rather than reported.
			adopt[key] = mustRecord(digest, now)
			continue
		}
		var was pinned
		if err := json.Unmarshal(raw, &was); err != nil || was.Digest == "" {
			adopt[key] = mustRecord(digest, now)
			continue
		}
		if was.Digest == digest {
			// Re-write to refresh the TTL: a definition served every day
			// should not expire out of the baseline and read as new.
			adopt[key] = mustRecord(digest, was.FirstSeen)
			continue
		}
		p.report(kind, name, was.Digest, digest)
		adopt[key] = mustRecord(digest, now)
	}
	if len(adopt) > 0 {
		p.store.SetMany(ctx, adopt, pinTTL)
	}
}

func mustRecord(digest string, firstSeen int64) []byte {
	// pinned has no field that can fail to marshal.
	data, _ := json.Marshal(pinned{Digest: digest, FirstSeen: firstSeen})
	return data
}

// pinKey namespaces a record by list kind and bare name. The store is already
// scoped per upstream by its provider scope, so the upstream id is not
// repeated here.
func pinKey(kind, name string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + name))
	return hex.EncodeToString(sum[:])
}

// checkTools and checkPrompts adapt the two list kinds whose definitions are
// instructions. Resources are deliberately absent: a resource URI is opaque
// and its content is data the caller asked for, not an instruction the model
// is handed unprompted.
func (p *definitionPins) checkTools(ctx context.Context, tools []*mcp.Tool) {
	if p == nil || p.report == nil {
		return
	}
	names := make([]string, 0, len(tools))
	digests := make([]string, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		names = append(names, t.Name)
		digests = append(digests, digestDefinition(t))
	}
	p.check(ctx, "tools", names, digests)
}

func (p *definitionPins) checkPrompts(ctx context.Context, prompts []*mcp.Prompt) {
	if p == nil || p.report == nil {
		return
	}
	names := make([]string, 0, len(prompts))
	digests := make([]string, 0, len(prompts))
	for _, pr := range prompts {
		if pr == nil {
			continue
		}
		names = append(names, pr.Name)
		digests = append(digests, digestDefinition(pr))
	}
	p.check(ctx, "prompts", names, digests)
}

// reportDrift is the reporting half, wired by the gateway so the upstream does
// not need to know about audit or metrics. Drift is not a request, so it does
// not pass through the middleware's exit door — but audit is where an operator
// looks, so it gets an event of its own with a method string that says what it
// is.
func (g *Gateway) reportDrift(u *upstream) func(kind, name, was, now string) {
	return func(kind, name, was, now string) {
		g.log.Warn("upstream definition changed",
			"upstream", u.cfg.ID, "kind", kind, "name", name,
			"was", was[:12], "now", now[:12])
		g.metrics.observeDrift(u.cfg.ID, kind)
		g.audit.Emit(audit.Event{
			Method:   "upstream/definitionChanged",
			Upstream: u.cfg.ID,
			Name:     name,
			Outcome:  audit.OutcomeWarned,
			// The digests, not the definitions: fold reports that something
			// changed, never what it now says. Reproducing the new text in
			// the trail would put upstream-controlled content into every
			// SIEM that reads it.
			Error: "definition digest " + was[:12] + " → " + now[:12] + " (" + kind + ")",
		})
	}
}
