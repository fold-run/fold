package gateway

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// startFleet runs two gateway instances over one upstream, sharing state
// through a single Redis — the deployment shape where a client's next
// request can land on an instance that never served its previous one.
func startFleet(t *testing.T, cfg *config.Config) (a, b *httptest.Server, gwA, gwB *Gateway) {
	t.Helper()
	mr := miniredis.RunT(t)
	start := func() (*httptest.Server, *Gateway) {
		c := *cfg
		server := *cfg.Server
		server.RedisURL = "redis://" + mr.Addr()
		c.Server = &server
		return startGateway(t, &c)
	}
	a, gwA = start()
	b, gwB = start()
	return a, b, gwA, gwB
}

// TestTaskOwnershipIsFleetWide: a task minted through one instance is bound
// to its minting principal on every instance. Without shared ownership the
// second instance has no record, falls through to the probe, and hands
// another principal the task — the isolation guarantee holding only for
// whichever instance happened to serve the mint.
func TestTaskOwnershipIsFleetWide(t *testing.T) {
	up := newTaskFixture(t, "a")
	iss := newFixtureIssuer(t)
	cfg := authedConfig(iss, []config.Upstream{{ID: "a", URL: up.URL, Namespace: "a"}}, nil)
	cfg.Server = &config.ServerSection{}
	tsA, tsB, _, _ := startFleet(t, cfg)

	aud := "https://gw.example.com"
	aliceToken := "Bearer " + iss.mint(t, "alice", aud, nil)
	bobToken := "Bearer " + iss.mint(t, "bob", aud, nil)

	// Alice mints on instance A.
	aliceA := taskClient(t, tsA.URL, map[string]string{"Authorization": aliceToken})
	out, err := aliceA.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "a__start_job", Arguments: map[string]any{},
	})
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

	// Bob asks instance B — which never saw the mint.
	bobB := taskClient(t, tsB.URL, map[string]string{"Authorization": bobToken})
	for _, m := range []string{"tasks/get", "tasks/cancel", "tasks/result", "tasks/update"} {
		if _, err := callTaskMethod(t, bobB, m, map[string]any{"taskId": taskID}); err == nil {
			t.Errorf("%s: bob reached alice's task through a second instance", m)
		} else if !strings.Contains(err.Error(), "no upstream owns") {
			t.Errorf("%s: denial should read like not-found, got: %v", m, err)
		}
	}

	// Bob's denials never reached the upstream: the task is still working.
	aliceB := taskClient(t, tsB.URL, map[string]string{"Authorization": aliceToken})
	res, err := callTaskMethod(t, aliceB, "tasks/get", map[string]any{"taskId": taskID})
	if err != nil {
		t.Fatalf("alice tasks/get on the second instance: %v", err)
	}
	if !strings.Contains(string(res), `"status":"working"`) {
		t.Errorf("alice's task on instance B = %s", res)
	}

	// tasks/list agrees on both instances.
	if ids := listedTaskIDs(t, bobB); len(ids) != 0 {
		t.Errorf("bob's tasks/list on instance B leaked: %v", ids)
	}
	if ids := listedTaskIDs(t, aliceB); len(ids) != 1 || ids[0] != taskID {
		t.Errorf("alice's tasks/list on instance B = %v, want [%s]", ids, taskID)
	}
}

// TestTaskOwnershipSurvivesInstanceRestart: shared ownership outlives the
// process that recorded it, so a rolling restart does not silently reopen
// every in-flight task to every caller.
func TestTaskOwnershipSurvivesInstanceRestart(t *testing.T) {
	up := newTaskFixture(t, "a")
	iss := newFixtureIssuer(t)
	mr := miniredis.RunT(t)

	cfg := authedConfig(iss, []config.Upstream{{ID: "a", URL: up.URL, Namespace: "a"}}, nil)
	cfg.Server = &config.ServerSection{RedisURL: "redis://" + mr.Addr()}

	aud := "https://gw.example.com"
	aliceToken := "Bearer " + iss.mint(t, "alice", aud, nil)
	bobToken := "Bearer " + iss.mint(t, "bob", aud, nil)

	tsA, gwA := startGateway(t, cfg)
	alice := taskClient(t, tsA.URL, map[string]string{"Authorization": aliceToken})
	out, err := alice.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "a__start_job", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("start_job: %v", err)
	}
	task, _ := out.Meta["task"].(map[string]any)
	taskID, _ := task["taskId"].(string)
	if taskID == "" {
		t.Fatalf("no task minted; meta=%v", out.Meta)
	}

	// The minting instance goes away entirely.
	_ = alice.Close()
	gwA.Close()
	tsA.Close()

	tsB, _ := startGateway(t, cfg)
	bob := taskClient(t, tsB.URL, map[string]string{"Authorization": bobToken})
	if _, err := callTaskMethod(t, bob, "tasks/get", map[string]any{"taskId": taskID}); err == nil {
		t.Fatal("ownership was forgotten across the restart: bob reached alice's task")
	}
	aliceB := taskClient(t, tsB.URL, map[string]string{"Authorization": aliceToken})
	if _, err := callTaskMethod(t, aliceB, "tasks/get", map[string]any{"taskId": taskID}); err != nil {
		t.Fatalf("the minter lost her own task across the restart: %v", err)
	}
}

// TestTaskOwnershipDegradesToPerInstance: with Redis unreachable mid-flight
// the gateway falls back to the records it mirrored locally rather than to
// no records at all — an outage must not reopen every task to every caller
// on the instance that holds them.
func TestTaskOwnershipSurvivesRedisOutage(t *testing.T) {
	up := newTaskFixture(t, "a")
	iss := newFixtureIssuer(t)
	mr := miniredis.RunT(t)

	cfg := authedConfig(iss, []config.Upstream{{ID: "a", URL: up.URL, Namespace: "a"}}, nil)
	cfg.Server = &config.ServerSection{RedisURL: "redis://" + mr.Addr()}
	ts, _ := startGateway(t, cfg)

	aud := "https://gw.example.com"
	alice := taskClient(t, ts.URL, map[string]string{"Authorization": "Bearer " + iss.mint(t, "alice", aud, nil)})
	bob := taskClient(t, ts.URL, map[string]string{"Authorization": "Bearer " + iss.mint(t, "bob", aud, nil)})

	out, err := alice.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "a__start_job", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("start_job: %v", err)
	}
	task, _ := out.Meta["task"].(map[string]any)
	taskID, _ := task["taskId"].(string)

	mr.Close() // Redis goes down under a running gateway

	if _, err := callTaskMethod(t, bob, "tasks/get", map[string]any{"taskId": taskID}); err == nil {
		t.Fatal("a Redis outage handed bob another principal's task")
	}
	if _, err := callTaskMethod(t, alice, "tasks/get", map[string]any{"taskId": taskID}); err != nil {
		t.Fatalf("a Redis outage locked the minter out of her own task: %v", err)
	}
}
