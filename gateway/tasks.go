package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/auth"
	"github.com/fold-run/fold/internal/state"
)

// Federated task support. MCP task ids are opaque and clients persist them
// across sessions, so — like resource URIs — fold never rewrites them.
// Ownership is remembered instead: a task minted through the gateway pins
// taskId → upstream, and a task fold never saw is located by probing.
//
// The Go SDK does not yet model the task lifecycle, so these methods are
// forwarded as opaque JSON via the SDK's custom-method mechanism, to fold's
// documented task contract. When the SDK ships typed task APIs this layer
// swaps its wire types for them; the routing is unchanged.
const (
	methodTasksGet    = "tasks/get"
	methodTasksList   = "tasks/list"
	methodTasksCancel = "tasks/cancel"
	methodTasksResult = "tasks/result"
	methodTasksUpdate = "tasks/update"

	// codeTaskNotFound is answered when no upstream owns a task id.
	codeTaskNotFound = -32002

	// metaMintedTask is the result _meta key an upstream sets to advertise a
	// task it just minted, letting fold pin affinity without a probe.
	metaMintedTask = "task"
)

// taskMethods are the request/response task methods fold federates.
// subscriptions/listen streams are not included: the SDK serves the
// 2026-07-28 protocol only on stateless HTTP servers, which fold's
// session-keyed bridging cannot use — see README "Not implemented" and the
// drift canary in listen_test.go.
var taskMethods = []string{
	methodTasksGet, methodTasksList, methodTasksCancel, methodTasksResult, methodTasksUpdate,
}

// taskOwnerTTL bounds how long an ownership record lives. A task outliving
// it degrades to the probe fallback — reachable by any caller — so the
// window is generous relative to any plausible task lifetime rather than
// tuned. It is not configurable: shortening it only weakens the binding,
// and lengthening it only costs storage.
const taskOwnerTTL = 24 * time.Hour

// taskRecord is one ownership entry: the upstream that owns the task plus a
// digest of the principal it was minted for. An empty owner means fold never
// saw the mint (out-of-band, or expired and rediscovered) — such tasks stay
// reachable by any caller, matching the locate-by-probe fallback.
type taskRecord struct {
	upstreamID string
	owner      string
}

// wireTaskRecord is the stored form. Short keys because every task in a
// federated list page is one of these.
type wireTaskRecord struct {
	Upstream string `json:"u"`
	Owner    string `json:"o,omitempty"`
}

// taskOwnerKey collapses a principal to the identity that owns its tasks.
// It is a digest, not the raw claims: the record is shared state, and the
// gateway does not put client-supplied identifiers into it verbatim (the
// same rule the per-principal rate limiter follows). Anonymous callers share
// one bucket, so no-auth deployments are unchanged.
func taskOwnerKey(p *auth.Principal) string {
	if p == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(p.Issuer + "\x00" + p.Subject))
	return hex.EncodeToString(sum[:])
}

// taskOwners is the taskId → (upstream, minting principal) index.
//
// It lives behind state.Provider rather than in process memory because it is
// an authorization record, not a routing hint: with Redis configured every
// instance of a fleet reads the same ownership, so a caller cannot reach
// another principal's task simply by landing on an instance that did not
// serve the mint. Without Redis it is the in-process store, which is exactly
// the previous behaviour.
type taskOwners struct{ store state.Store }

// key hashes the task id. Ids are upstream-minted, but a caller names one
// freely on lookup, so it never becomes a shared-store key verbatim.
func (t *taskOwners) key(taskID string) string {
	sum := sha256.Sum256([]byte(taskID))
	return hex.EncodeToString(sum[:])
}

func (t *taskOwners) load(ctx context.Context, taskID string) (taskRecord, bool) {
	data, ok := t.store.Get(ctx, t.key(taskID))
	if !ok {
		return taskRecord{}, false
	}
	return decodeTaskRecord(data)
}

// loadMany resolves a whole page of task ids in one round trip; ids with no
// record are absent from the result.
func (t *taskOwners) loadMany(ctx context.Context, taskIDs []string) map[string]taskRecord {
	keys := make([]string, 0, len(taskIDs))
	byKey := make(map[string]string, len(taskIDs))
	for _, id := range taskIDs {
		k := t.key(id)
		if _, dup := byKey[k]; dup {
			continue
		}
		byKey[k] = id
		keys = append(keys, k)
	}
	out := make(map[string]taskRecord, len(keys))
	for k, data := range t.store.GetMany(ctx, keys) {
		if rec, ok := decodeTaskRecord(data); ok {
			out[byKey[k]] = rec
		}
	}
	return out
}

// put writes a record as given, refreshing its lifetime.
func (t *taskOwners) put(ctx context.Context, taskID string, rec taskRecord) {
	if data, err := json.Marshal(wireTaskRecord{Upstream: rec.upstreamID, Owner: rec.owner}); err == nil {
		t.store.Set(ctx, t.key(taskID), data, taskOwnerTTL)
	}
}

// putMany writes a batch of records in one round trip.
func (t *taskOwners) putMany(ctx context.Context, recs map[string]taskRecord) {
	if len(recs) == 0 {
		return
	}
	entries := make(map[string][]byte, len(recs))
	for id, rec := range recs {
		if data, err := json.Marshal(wireTaskRecord{Upstream: rec.upstreamID, Owner: rec.owner}); err == nil {
			entries[t.key(id)] = data
		}
	}
	t.store.SetMany(ctx, entries, taskOwnerTTL)
}

func (t *taskOwners) delete(ctx context.Context, taskID string) {
	t.store.Delete(ctx, t.key(taskID))
}

func decodeTaskRecord(data []byte) (taskRecord, bool) {
	var w wireTaskRecord
	if err := json.Unmarshal(data, &w); err != nil || w.Upstream == "" {
		return taskRecord{}, false
	}
	return taskRecord{upstreamID: w.Upstream, owner: w.Owner}, true
}

// mergeTaskOwner applies the ownership rule: an owner already on record is
// preserved, so discovery via list or probe updates routing but never
// reassigns ownership to the discovering caller.
func mergeTaskOwner(prev taskRecord, found bool, upstreamID, owner string) taskRecord {
	if found && prev.owner != "" {
		owner = prev.owner
	}
	return taskRecord{upstreamID: upstreamID, owner: owner}
}

// storeTaskAffinity pins taskID → upstream, preserving any owner on record.
func (g *Gateway) storeTaskAffinity(ctx context.Context, taskID, upstreamID, owner string) {
	prev, found := g.taskOwner.load(ctx, taskID)
	g.taskOwner.put(ctx, taskID, mergeTaskOwner(prev, found, upstreamID, owner))
}

// rawParams forwards an opaque JSON params object through the SDK custom
// method machinery.
type rawParams struct {
	mcp.ParamsBase
	raw json.RawMessage
}

func (p *rawParams) MarshalJSON() ([]byte, error) {
	if len(p.raw) == 0 {
		return []byte("{}"), nil
	}
	return p.raw, nil
}
func (p *rawParams) UnmarshalJSON(b []byte) error {
	p.raw = append(p.raw[:0], b...)
	return nil
}

// rawResult forwards an opaque JSON result object.
type rawResult struct {
	mcp.ResultBase
	raw json.RawMessage
}

func (r *rawResult) MarshalJSON() ([]byte, error) {
	if len(r.raw) == 0 {
		return []byte("{}"), nil
	}
	return r.raw, nil
}
func (r *rawResult) UnmarshalJSON(b []byte) error {
	r.raw = append(r.raw[:0], b...)
	return nil
}

// registerTaskMethods wires the task methods as receiving custom methods on
// the gateway server so they dispatch through the middleware chain to the
// federation router.
func (g *Gateway) registerTaskMethods() error {
	for _, method := range taskMethods {
		if err := mcp.AddReceivingCustomMethod(g.server, method,
			func(ctx context.Context, _ *mcp.ServerSession, p *rawParams) (*rawResult, error) {
				var raw json.RawMessage
				if p != nil {
					raw = p.raw
				}
				out, err := g.routeTask(ctx, method, raw)
				if err != nil {
					return nil, err
				}
				return &rawResult{raw: out}, nil
			}); err != nil {
			return fmt.Errorf("register task method %s: %w", method, err)
		}
	}
	return nil
}

// extractTaskID reads {"taskId": "..."} from a task params object.
func extractTaskID(raw json.RawMessage) string {
	var p struct {
		TaskID string `json:"taskId"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.TaskID
}

// routeTask dispatches one task method. tasks/list fans out; the others
// route to the owning upstream by affinity, falling back to a probe.
func (g *Gateway) routeTask(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error) {
	rt := g.rt()
	// Task params reach the upstream as the bytes the caller sent, so the
	// connection-owned `_meta` keys the typed paths strip have to be removed
	// here too — and here fold is the only thing that can, because the SDK
	// does not model these methods and so never stamps its own values over
	// them. Done once, before any branch: every route below forwards this.
	raw = sanitizeRawMeta(raw)
	caller := taskOwnerKey(auth.PrincipalFromContext(ctx))
	if method == methodTasksList {
		return g.listTasks(ctx, rt, caller, raw)
	}
	taskID := extractTaskID(raw)
	if taskID == "" {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "task method requires a taskId"}
	}

	// Affinity: route straight to the remembered owner. Its own errors
	// (including a genuine not-found) pass through verbatim. A task minted
	// for a different principal is answered exactly like an unknown id —
	// the denial must not reveal existence — and is never probed or
	// forwarded on this caller's behalf.
	if rec, ok := g.taskOwner.load(ctx, taskID); ok {
		if rec.owner != "" && rec.owner != caller {
			return nil, &jsonrpc.Error{Code: codeTaskNotFound, Message: fmt.Sprintf("no upstream owns task %q", taskID)}
		}
		// A task held by an upstream outside the caller's tenant subset is
		// answered exactly like an unknown id — the same posture this path
		// already takes for another principal's task, and for the same
		// reason: the refusal must not reveal existence.
		if !tenantFrom(ctx).sees(rec.upstreamID) {
			return nil, &jsonrpc.Error{Code: codeTaskNotFound, Message: fmt.Sprintf("no upstream owns task %q", taskID)}
		}
		if u := rt.byID[rec.upstreamID]; u != nil {
			g.taskOwner.put(ctx, taskID, rec) // refresh on use
			return u.callTask(ctx, method, raw)
		}
	}

	// Probe fallback: locate the owner with a read-only tasks/get across
	// upstreams — the owner answers, everyone else is a "no". Never fan a
	// mutating method (cancel/result/update) out; locate first, then act on
	// the owner alone. Fold cannot know who minted a task it never saw, so
	// the record stays ownerless (out-of-band sharing keeps working).
	owner := g.locateTaskOwner(ctx, rt, taskID)
	if owner == nil {
		return nil, &jsonrpc.Error{Code: codeTaskNotFound, Message: fmt.Sprintf("no upstream owns task %q", taskID)}
	}
	g.storeTaskAffinity(ctx, taskID, owner.cfg.ID, "")
	return owner.callTask(ctx, method, raw)
}

// locateTaskOwner probes every upstream with tasks/get and returns the one
// that recognizes the task, or nil.
func (g *Gateway) locateTaskOwner(ctx context.Context, rt *routes, taskID string) *upstream {
	probe, _ := json.Marshal(map[string]string{"taskId": taskID})
	// Probe only what the caller's tenant may reach: a task id guessed from
	// another tenant's stream must not locate its owner here.
	ups := visibleUpstreams(ctx, rt.upstreams)
	results, _ := fanOut(ctx, ups, func(ctx context.Context, u *upstream) (json.RawMessage, error) {
		return u.callTask(ctx, methodTasksGet, probe)
	})
	for i, r := range results {
		if r != nil {
			return ups[i]
		}
	}
	return nil
}

// listTasks merges tasks/list across every upstream, filtered to the calling
// principal: upstreams see fold as one client, so the merged list would
// otherwise expose every caller's tasks. Tasks with no ownership record
// (out of band, or expired) are kept, matching the locate-by-probe fallback.
// A partial-failure marker records any upstreams that were down. The merged
// list is sorted by task id and served in pages with the same snapshot-offset
// cursors as the typed lists (see paginate).
func (g *Gateway) listTasks(ctx context.Context, rt *routes, caller string, raw json.RawMessage) (json.RawMessage, error) {
	empty, _ := json.Marshal(map[string]any{})
	ups := visibleUpstreams(ctx, rt.upstreams)
	results, failed := fanOut(ctx, ups, func(ctx context.Context, u *upstream) (json.RawMessage, error) {
		return u.callTask(ctx, methodTasksList, empty)
	})
	meta, err := partialFailureMeta(failed, len(ups))
	if err != nil {
		return nil, err
	}
	// Collect first, resolve ownership in one batch. A round trip per task
	// would make list latency scale with the federation's task set, which
	// with shared state is a network cost, not a map lookup.
	type listed struct {
		task     json.RawMessage
		id       string
		upstream string
	}
	var all []listed
	var ids []string
	for i, r := range results {
		if r == nil {
			continue
		}
		var page struct {
			Tasks []json.RawMessage `json:"tasks"`
		}
		if err := json.Unmarshal(r, &page); err != nil {
			continue
		}
		for _, t := range page.Tasks {
			id := extractTaskID(t)
			all = append(all, listed{task: t, id: id, upstream: ups[i].cfg.ID})
			if id != "" {
				ids = append(ids, id)
			}
		}
	}
	known := g.taskOwner.loadMany(ctx, ids)

	// Pin affinity for every listed task to the upstream that returned it
	// (never reassigning ownership), then hide other callers' tasks.
	writes := map[string]taskRecord{}
	merged := []json.RawMessage{}
	for _, l := range all {
		if l.id != "" {
			prev, found := known[l.id]
			rec := mergeTaskOwner(prev, found, l.upstream, "")
			if !found || prev != rec {
				writes[l.id] = rec
			}
			if rec.owner != "" && rec.owner != caller {
				continue
			}
		}
		merged = append(merged, l.task)
	}
	g.taskOwner.putMany(ctx, writes)
	// Deterministic order so a cursor's offset means the same thing on the
	// next page: upstreams commonly emit task lists in map order, and the
	// snapshot fingerprint would treat that jitter as a changed snapshot.
	// Ids are unique across the federation (upstream-minted), so id order is
	// total; raw bytes break ties for malformed entries without an id.
	slices.SortStableFunc(merged, func(a, b json.RawMessage) int {
		if c := strings.Compare(extractTaskID(a), extractTaskID(b)); c != 0 {
			return c
		}
		return bytes.Compare(a, b)
	})
	var params struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(raw, &params) // absent params → first page
	page, next, jerr := paginate(merged, extractTaskID, "tasks", params.Cursor,
		g.pageSize, auth.PrincipalFromContext(ctx))
	if jerr != nil {
		return nil, jerr
	}
	out := map[string]any{"tasks": page}
	if next != "" {
		out["nextCursor"] = next
	}
	if meta != nil {
		out["_meta"] = meta
	}
	return json.Marshal(out)
}

// noteMintedTask pins affinity when a tools/call result advertises a task it
// created, so later task calls skip the probe. The mint records the calling
// principal as the task's owner: task-scoped calls and tasks/list answer only
// that principal from then on.
func (g *Gateway) noteMintedTask(ctx context.Context, u *upstream, meta mcp.Meta) {
	if meta == nil {
		return
	}
	task, ok := meta[metaMintedTask].(map[string]any)
	if !ok {
		return
	}
	if id, ok := task["taskId"].(string); ok && id != "" {
		g.storeTaskAffinity(ctx, id, u.cfg.ID, taskOwnerKey(auth.PrincipalFromContext(ctx)))
	}
}
