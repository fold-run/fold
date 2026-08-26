package gateway

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// MCP's caching rules (specification 2026-07-28, "Caching") require a server
// to put a freshness hint on every complete list result, and define exactly
// two values for its scope: "public", meaning any client or shared proxy may
// serve the response to any user, and "private", meaning a cache MUST NOT be
// shared across authorization contexts.
//
// fold builds its list results itself — federated, merged, and filtered — so
// it never reaches the SDK's own handlers, which are what fill these fields in
// for an ordinary server. Left alone, every federated list went out with
// `"cacheScope": ""`, which is neither permitted value. A client applying the
// "absent means public" default to an empty string would then treat a list
// fold had *filtered per principal* as shareable with anyone, which is the
// specification's own example of what must be private:
//
//	"private" is appropriate for [...] filtered list results that vary per user.
//
// That never became an incident because the accompanying `ttlMs` is 0 — the
// spec's "immediately stale" — so a conforming client re-fetches anyway. But
// the two hints disagreed, and the one that would have contained the damage
// is the one fold was not deliberately setting.
const (
	cacheScopePublic  = "public"
	cacheScopePrivate = "private"
)

// listCacheScope is the scope fold's own list results carry, computed once per
// routing snapshot because it depends only on configuration.
//
// The question it answers is narrow: can two callers legitimately receive
// different lists from this gateway? Whenever they can, the response is not
// shareable between them and the scope is private. fold says public only for
// the federation that genuinely serves one list to everyone.
func listCacheScope(cfg *config.Config, upstreams []*upstream) string {
	if cfg == nil {
		return cacheScopePublic
	}
	// Policy filters lists per principal — the invisibility half of the
	// enforcement pair — so a document with any rule makes the list a
	// per-caller view.
	//
	// `defaultDecision` deliberately does not appear here, and the reason is
	// the whole question this function asks. A default with no rules serves
	// *every* caller the same list: the entire federation under "allow", and
	// nothing at all under "deny". Neither varies by who is asking, so both
	// are shareable. Reading the default as evidence of per-caller filtering
	// would also have split two documents that behave identically — `{}` and
	// `{"defaultDecision": "deny"}` are the same deny-all engine, since
	// policy.New sets defaultAllow only for the literal "allow" — and given
	// them different scopes.
	if cfg.Policy != nil && len(cfg.Policy.Rules) > 0 {
		return cacheScopePrivate
	}
	// Tenants bound visibility to an upstream subset before policy runs.
	if len(cfg.Tenants) > 0 {
		return cacheScopePrivate
	}
	// A caller-derived credential means the upstream itself may answer
	// differently per principal, which is why fold already declines to cache
	// those lists at all (see upstream.callerDerived).
	for _, u := range upstreams {
		if u.callerDerived() {
			return cacheScopePrivate
		}
	}
	return cacheScopePublic
}

// applyCacheHints stamps fold's caching hints onto a list result.
//
// ttlMs is deliberately left at 0 — "immediately stale", the value fold has
// always sent. Advertising a positive lifetime would tell clients to hold a
// list across a window in which a reload, a discovery sync, or a policy change
// can alter what the caller is entitled to see. fold does emit list_changed on
// all three, which is the invalidation signal the spec pairs with a TTL, but
// choosing a number here would be choosing it for every deployment at once.
// The scope is the half that was wrong, and it is the half being fixed.
func applyCacheHints(c *mcp.Cacheable, scope string) {
	c.CacheScope = scope
}

// One invariant this file does not enforce but the next cache will have to.
//
// The specification forbids caching a result produced by an input-required
// retry: results from "requests carrying `inputResponses` or `requestState`
// MUST NOT be cached, as they depend on inputs that are not part of the cache
// key". fold satisfies that today by accident of coverage rather than by
// design — its only cache holds list results, and the multi-round-trip pattern
// applies to `tools/call`, `prompts/get` and `resources/read`, none of which
// fold caches.
//
// So the rule has never had to be written down, and a response cache on any of
// those three methods would break it on its first day. Whoever adds one:
// a request carrying either field is not cacheable, and neither is its result.
