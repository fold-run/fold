package gateway

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/auth"
	"github.com/fold-run/fold/policy"
)

// metaTruncated marks a list that a policy rule's maxItems cut short. A cap
// that hid capability silently would be worse than no cap: an agent would
// simply stop being able to do things, with nothing anywhere saying why.
const metaTruncated = "run.fold/truncated"

// listFilter applies policy visibility and per-rule caps to one merged list.
//
// fold has always filtered lists per principal — a caller never sees a tool
// they cannot call — which is the structural answer to "context bloat" that
// needs no model in the loop. Two things were missing: the reduction was
// invisible, and it was unbounded. This adds both, and nothing else: the
// decision it consults is the one the filter already made.
//
// The cap drops whatever falls past it in merge order. That is a bound rather
// than a curation, and deliberately so — fold has no notion of which tools
// matter, and acquiring one would be the semantic tool selection the roadmap
// declines. Which items survive is stable for a given snapshot, since the
// merge order is upstream order and each upstream's own list order.
type listFilter struct {
	principal *auth.Principal
	policy    *policy.Engine
	method    string // the invocation the list gates on, e.g. "tools/call"

	// admitted counts what each rule has let through for this request, which
	// is what a per-rule cap is counted against.
	admitted map[string]int

	offered int // items the upstreams returned
	served  int // items the caller receives
	capped  int // items dropped by a cap rather than by policy
}

func newListFilter(principal *auth.Principal, pol *policy.Engine, method string) *listFilter {
	return &listFilter{principal: principal, policy: pol, method: method}
}

// visible reports whether one item survives policy and the rule's cap.
func (f *listFilter) visible(upstreamID, name string) bool {
	f.offered++
	d := f.policy.Decide(f.principal, upstreamID, f.method, name)
	if !d.Allowed {
		return false
	}
	if d.MaxItems > 0 {
		// Rules with no cap never touch the map, so the common configuration
		// allocates nothing here.
		if f.admitted == nil {
			f.admitted = make(map[string]int, 1)
		}
		if f.admitted[d.RuleID] >= d.MaxItems {
			f.capped++
			return false
		}
		f.admitted[d.RuleID]++
	}
	f.served++
	return true
}

// finish records what the filtering did: on the metric, in the audit event,
// and — when a cap actually bit — in the result's _meta, where the client can
// see that its list is short by construction rather than by accident.
func (f *listFilter) finish(g *Gateway, evt *audit.Event, meta *mcp.Meta) {
	g.metrics.observeListShaping(f.method, f.offered, f.served, f.capped)
	if evt != nil {
		evt.ItemsCapped = f.capped
	}
	if f.capped == 0 || meta == nil {
		return
	}
	if *meta == nil {
		*meta = mcp.Meta{}
	}
	(*meta)[metaTruncated] = map[string]any{
		"dropped": f.capped,
		"reason":  "policy maxItems",
	}
}
