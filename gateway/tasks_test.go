package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// taskFixture is an MCP server that mints tasks from a tool call and answers
// the task lifecycle methods for the ids it owns.
type taskFixture struct {
	server *mcp.Server
	mu     sync.Mutex
	tasks  map[string]string // taskId → status
	prefix string
	nextID int
}

func newTaskFixture(t *testing.T, prefix string) *httptest.Server {
	t.Helper()
	f := &taskFixture{tasks: map[string]string{}, prefix: prefix}
	f.server = mcp.NewServer(&mcp.Implementation{Name: "task-fixture-" + prefix, Version: "1.0"}, nil)

	// A tool that mints a task and advertises it in the result _meta.
	f.server.AddTool(&mcp.Tool{Name: "start_job", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			f.mu.Lock()
			id := fmt.Sprintf("%s-task-%d", f.prefix, f.nextID)
			f.nextID++
			f.tasks[id] = "working"
			f.mu.Unlock()
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "started " + id}},
				Meta:    mcp.Meta{"task": map[string]any{"taskId": id, "status": "working"}},
			}, nil
		})

	owns := func(raw json.RawMessage) (string, bool) {
		var p struct {
			TaskID string `json:"taskId"`
		}
		json.Unmarshal(raw, &p)
		f.mu.Lock()
		defer f.mu.Unlock()
		st, ok := f.tasks[p.TaskID]
		return st, ok
	}
	notFound := &jsonrpc.Error{Code: -32002, Message: "task not found"}

	mcp.AddReceivingCustomMethod(f.server, "tasks/get", func(ctx context.Context, ss *mcp.ServerSession, p *rawParams) (*rawResult, error) {
		st, ok := owns(p.raw)
		if !ok {
			return nil, notFound
		}
		id := extractTaskID(p.raw)
		return &rawResult{raw: json.RawMessage(fmt.Sprintf(`{"task":{"taskId":%q,"status":%q}}`, id, st))}, nil
	})
	mcp.AddReceivingCustomMethod(f.server, "tasks/cancel", func(ctx context.Context, ss *mcp.ServerSession, p *rawParams) (*rawResult, error) {
		id := extractTaskID(p.raw)
		f.mu.Lock()
		_, ok := f.tasks[id]
		if ok {
			f.tasks[id] = "cancelled"
		}
		f.mu.Unlock()
		if !ok {
			return nil, notFound
		}
		return &rawResult{raw: json.RawMessage(fmt.Sprintf(`{"task":{"taskId":%q,"status":"cancelled"}}`, id))}, nil
	})
	mcp.AddReceivingCustomMethod(f.server, "tasks/result", func(ctx context.Context, ss *mcp.ServerSession, p *rawParams) (*rawResult, error) {
		if _, ok := owns(p.raw); !ok {
			return nil, notFound
		}
		return &rawResult{raw: json.RawMessage(`{"content":[{"type":"text","text":"done"}]}`)}, nil
	})
	mcp.AddReceivingCustomMethod(f.server, "tasks/update", func(ctx context.Context, ss *mcp.ServerSession, p *rawParams) (*rawResult, error) {
		if _, ok := owns(p.raw); !ok {
			return nil, notFound
		}
		return &rawResult{raw: json.RawMessage(`{"ok":true}`)}, nil
	})
	mcp.AddReceivingCustomMethod(f.server, "tasks/list", func(ctx context.Context, ss *mcp.ServerSession, p *rawParams) (*rawResult, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var items []string
		for id, st := range f.tasks {
			items = append(items, fmt.Sprintf(`{"taskId":%q,"status":%q}`, id, st))
		}
		return &rawResult{raw: json.RawMessage(fmt.Sprintf(`{"tasks":[%s]}`, strings.Join(items, ",")))}, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return f.server }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

// callTaskMethod drives a task method through the gateway from a client.
func callTaskMethod(t *testing.T, session *mcp.ClientSession, method string, params map[string]any) (json.RawMessage, error) {
	t.Helper()
	raw, _ := json.Marshal(params)
	res, err := mcp.CallCustomMethod[*rawParams, *rawResult](context.Background(), session, method, &rawParams{raw: raw})
	if err != nil {
		return nil, err
	}
	return res.raw, nil
}

func taskClient(t *testing.T, url string, headers map[string]string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "task-client", Version: "1.0"}, nil)
	for _, m := range taskMethods {
		if err := mcp.AddSendingCustomMethod[*rawParams, *rawResult](client, m); err != nil {
			t.Fatal(err)
		}
	}
	httpClient := http.DefaultClient
	if headers != nil {
		httpClient = &http.Client{Transport: headerTransport{headers: headers}}
	}
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   url + "/mcp",
		HTTPClient: httpClient,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func TestFederatedTasks(t *testing.T) {
	upA := newTaskFixture(t, "a")
	upB := newTaskFixture(t, "b")
	ts, gw := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "a", URL: upA.URL, Namespace: "a"},
		{ID: "b", URL: upB.URL, Namespace: "b"},
	}})
	session := taskClient(t, ts.URL, nil)

	// Mint a task on upstream A via a tool call. Affinity is pinned from the
	// result _meta.
	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "a__start_job", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("start_job: %v", err)
	}
	taskID := ""
	if task, ok := out.Meta["task"].(map[string]any); ok {
		taskID, _ = task["taskId"].(string)
	}
	if taskID == "" {
		t.Fatalf("no task minted; meta=%v", out.Meta)
	}
	if rec, ok := gw.taskOwner.Load(taskID); !ok || rec.(taskRecord).upstreamID != "a" {
		t.Errorf("affinity not pinned to a: %v", rec)
	}

	// tasks/get routes to the owner by affinity.
	res, err := callTaskMethod(t, session, "tasks/get", map[string]any{"taskId": taskID})
	if err != nil {
		t.Fatalf("tasks/get: %v", err)
	}
	if !strings.Contains(string(res), `"status":"working"`) {
		t.Errorf("tasks/get = %s", res)
	}

	// A task fold never saw (evict the affinity) is found by probe fallback.
	gw.taskOwner.Delete(taskID)
	res, err = callTaskMethod(t, session, "tasks/get", map[string]any{"taskId": taskID})
	if err != nil {
		t.Fatalf("tasks/get after eviction: %v", err)
	}
	if !strings.Contains(string(res), taskID) {
		t.Errorf("probe fallback failed: %s", res)
	}
	if rec, ok := gw.taskOwner.Load(taskID); !ok || rec.(taskRecord).upstreamID != "a" {
		t.Errorf("probe did not re-pin affinity: %v", rec)
	}

	// A mutating method (cancel) reaches only the owner.
	if _, err := callTaskMethod(t, session, "tasks/cancel", map[string]any{"taskId": taskID}); err != nil {
		t.Fatalf("tasks/cancel: %v", err)
	}
	res, _ = callTaskMethod(t, session, "tasks/get", map[string]any{"taskId": taskID})
	if !strings.Contains(string(res), `"status":"cancelled"`) {
		t.Errorf("cancel did not take on owner: %s", res)
	}

	// An unknown task id answers -32002 from no upstream.
	_, err = callTaskMethod(t, session, "tasks/get", map[string]any{"taskId": "nonexistent"})
	if err == nil {
		t.Error("expected error for unknown task id")
	} else if !strings.Contains(err.Error(), "-32002") && !strings.Contains(err.Error(), "no upstream owns") {
		t.Errorf("unexpected error for unknown task: %v", err)
	}

	// Mint a task on B too, then tasks/list merges across the federation.
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "b__start_job", Arguments: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	list, err := callTaskMethod(t, session, "tasks/list", nil)
	if err != nil {
		t.Fatalf("tasks/list: %v", err)
	}
	var page struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(list, &page); err != nil {
		t.Fatal(err)
	}
	var sawA, sawB bool
	for _, tk := range page.Tasks {
		id, _ := tk["taskId"].(string)
		if strings.HasPrefix(id, "a-task") {
			sawA = true
		}
		if strings.HasPrefix(id, "b-task") {
			sawB = true
		}
	}
	if !sawA || !sawB {
		t.Errorf("tasks/list did not merge both upstreams (a=%v b=%v): %s", sawA, sawB, list)
	}
}

// listedTaskIDs runs tasks/list and returns the task ids the caller can see.
func listedTaskIDs(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	list, err := callTaskMethod(t, session, "tasks/list", nil)
	if err != nil {
		t.Fatalf("tasks/list: %v", err)
	}
	var page struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal(list, &page); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, tk := range page.Tasks {
		if id, _ := tk["taskId"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// A task minted by one principal is invisible to every other: task-scoped
// calls answer -32002 exactly like an unknown id (no existence leak, no
// probe, no upstream contact), and tasks/list hides it. Tasks fold has no
// ownership record for (out-of-band or evicted) stay reachable by anyone,
// matching the locate-by-probe fallback.
func TestTaskPrincipalIsolation(t *testing.T) {
	up := newTaskFixture(t, "a")
	iss := newFixtureIssuer(t)
	ts, gw := startGateway(t, authedConfig(iss,
		[]config.Upstream{{ID: "a", URL: up.URL, Namespace: "a"}}, nil))

	aud := "https://gw.example.com"
	alice := taskClient(t, ts.URL, map[string]string{"Authorization": "Bearer " + iss.mint(t, "alice", aud, nil)})
	bob := taskClient(t, ts.URL, map[string]string{"Authorization": "Bearer " + iss.mint(t, "bob", aud, nil)})

	out, err := alice.CallTool(context.Background(), &mcp.CallToolParams{Name: "a__start_job", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("start_job: %v", err)
	}
	taskID := ""
	if task, ok := out.Meta["task"].(map[string]any); ok {
		taskID, _ = task["taskId"].(string)
	}
	if taskID == "" {
		t.Fatalf("no task minted; meta=%v", out.Meta)
	}

	// Bob cannot read or cancel alice's task, and the denial is
	// indistinguishable from an unknown task id.
	for _, m := range []string{"tasks/get", "tasks/cancel", "tasks/result", "tasks/update"} {
		if _, err := callTaskMethod(t, bob, m, map[string]any{"taskId": taskID}); err == nil {
			t.Errorf("%s: bob reached alice's task", m)
		} else if !strings.Contains(err.Error(), "no upstream owns") {
			t.Errorf("%s: denial should read like not-found, got: %v", m, err)
		}
	}
	// The denials never reached the upstream: bob's cancel did not take.
	res, err := callTaskMethod(t, alice, "tasks/get", map[string]any{"taskId": taskID})
	if err != nil {
		t.Fatalf("alice tasks/get: %v", err)
	}
	if !strings.Contains(string(res), `"status":"working"`) {
		t.Errorf("bob's denied cancel reached the upstream: %s", res)
	}

	// tasks/list shows the task to alice only.
	if ids := listedTaskIDs(t, bob); len(ids) != 0 {
		t.Errorf("bob's tasks/list leaked: %v", ids)
	}
	if ids := listedTaskIDs(t, alice); len(ids) != 1 || ids[0] != taskID {
		t.Errorf("alice's tasks/list = %v, want [%s]", ids, taskID)
	}

	// A task with no ownership record (fold restarted, or minted out of
	// band) stays reachable by any caller via the probe fallback, and the
	// probe never claims ownership for the prober.
	gw.taskOwner.Delete(taskID)
	if _, err := callTaskMethod(t, bob, "tasks/get", map[string]any{"taskId": taskID}); err != nil {
		t.Errorf("unowned task should be probe-reachable: %v", err)
	}
	if v, ok := gw.taskOwner.Load(taskID); !ok || v.(taskRecord).owner != "" {
		t.Errorf("probe must not assign ownership: %+v", v)
	}
	if _, err := callTaskMethod(t, alice, "tasks/get", map[string]any{"taskId": taskID}); err != nil {
		t.Errorf("minter locked out of rediscovered task: %v", err)
	}
}

// TestTasksListPagination: the merged federated task list serves in pages —
// deterministic id order, cursor-linked, invalid cursors rejected — matching
// the typed lists' snapshot-offset contract.
func TestTasksListPagination(t *testing.T) {
	upA := newTaskFixture(t, "a")
	upB := newTaskFixture(t, "b")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{
			{ID: "a", URL: upA.URL, Namespace: "a"},
			{ID: "b", URL: upB.URL, Namespace: "b"},
		},
		Routing: &config.Routing{PageSize: 2},
	})
	session := taskClient(t, ts.URL, nil)

	// Five tasks across the federation: a-task-0..2, b-task-0..1.
	for range 3 {
		if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "a__start_job"}); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "b__start_job"}); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	cursor := ""
	pages := 0
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := callTaskMethod(t, session, "tasks/list", params)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		var page struct {
			Tasks      []json.RawMessage `json:"tasks"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &page); err != nil {
			t.Fatal(err)
		}
		pages++
		if len(page.Tasks) > 2 {
			t.Fatalf("page %d has %d tasks, page size is 2", pages, len(page.Tasks))
		}
		for _, task := range page.Tasks {
			got = append(got, extractTaskID(task))
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("cursor walk did not terminate")
		}
	}

	want := []string{"a-task-0", "a-task-1", "a-task-2", "b-task-0", "b-task-1"}
	if pages != 3 {
		t.Errorf("walked %d pages, want 3", pages)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("paged ids = %v, want %v (deterministic id order, no dupes)", got, want)
	}

	// A garbage cursor is rejected, telling the client to restart the list.
	if _, err := callTaskMethod(t, session, "tasks/list", map[string]any{"cursor": "not-a-cursor"}); err == nil {
		t.Error("garbage cursor should be rejected")
	} else if !strings.Contains(err.Error(), "cursor") {
		t.Errorf("unexpected error: %v", err)
	}
}
