package gateway

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
// subscriptions/listen fan-in is not included (the SDK exposes no public
// API for it).
var taskMethods = []string{
	methodTasksGet, methodTasksList, methodTasksCancel, methodTasksResult, methodTasksUpdate,
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
func (g *Gateway) registerTaskMethods() {
	for _, method := range taskMethods {
		method := method
		mcp.AddReceivingCustomMethod(g.server, method,
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
			})
	}
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
	if method == methodTasksList {
		return g.listTasks(ctx)
	}
	taskID := extractTaskID(raw)
	if taskID == "" {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "task method requires a taskId"}
	}

	// Affinity: route straight to the remembered owner. Its own errors
	// (including a genuine not-found) pass through verbatim.
	if id, ok := g.taskOwner.Load(taskID); ok {
		if u := g.byID[id.(string)]; u != nil {
			g.taskOwner.Store(taskID, u.cfg.ID) // refresh on use
			return u.callTask(ctx, method, raw)
		}
	}

	// Probe fallback: locate the owner with a read-only tasks/get across
	// upstreams — the owner answers, everyone else is a "no". Never fan a
	// mutating method (cancel/result/update) out; locate first, then act on
	// the owner alone.
	owner := g.locateTaskOwner(ctx, taskID)
	if owner == nil {
		return nil, &jsonrpc.Error{Code: codeTaskNotFound, Message: fmt.Sprintf("no upstream owns task %q", taskID)}
	}
	g.taskOwner.Store(taskID, owner.cfg.ID)
	return owner.callTask(ctx, method, raw)
}

// locateTaskOwner probes every upstream with tasks/get and returns the one
// that recognizes the task, or nil.
func (g *Gateway) locateTaskOwner(ctx context.Context, taskID string) *upstream {
	probe, _ := json.Marshal(map[string]string{"taskId": taskID})
	results, _ := fanOut(ctx, g.upstreams, func(ctx context.Context, u *upstream) (json.RawMessage, error) {
		return u.callTask(ctx, methodTasksGet, probe)
	})
	for i, r := range results {
		if r != nil {
			return g.upstreams[i]
		}
	}
	return nil
}

// listTasks merges tasks/list across every upstream. Ids no upstream knows
// simply do not appear; a partial-failure marker records any that were down.
func (g *Gateway) listTasks(ctx context.Context) (json.RawMessage, error) {
	empty, _ := json.Marshal(map[string]any{})
	results, failed := fanOut(ctx, g.upstreams, func(ctx context.Context, u *upstream) (json.RawMessage, error) {
		return u.callTask(ctx, methodTasksList, empty)
	})
	meta, err := partialFailureMeta(failed, len(g.upstreams))
	if err != nil {
		return nil, err
	}
	merged := []json.RawMessage{}
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
		// Pin affinity for every listed task to the upstream that returned it.
		for _, t := range page.Tasks {
			if id := extractTaskID(t); id != "" {
				g.taskOwner.Store(id, g.upstreams[i].cfg.ID)
			}
			merged = append(merged, t)
		}
	}
	out := map[string]any{"tasks": merged}
	if meta != nil {
		out["_meta"] = meta
	}
	return json.Marshal(out)
}

// noteMintedTask pins affinity when a tools/call result advertises a task it
// created, so later task calls skip the probe.
func (g *Gateway) noteMintedTask(u *upstream, meta mcp.Meta) {
	if meta == nil {
		return
	}
	task, ok := meta[metaMintedTask].(map[string]any)
	if !ok {
		return
	}
	if id, ok := task["taskId"].(string); ok && id != "" {
		g.taskOwner.Store(id, u.cfg.ID)
	}
}
