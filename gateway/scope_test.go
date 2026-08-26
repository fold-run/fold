package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/config"
)

// Scope-gated policy end to end. A scope is the one thing a denial is allowed
// to name, because unlike an argument's expected value it is a credential the
// caller goes and obtains — so the interesting assertions here are about what
// the refusal says, not only that it refuses.

// scopedDocsGateway starts an authenticated gateway in front of one real SDK
// upstream, governed by a document where reading needs one scope and editing
// needs two. It returns a minter for tokens carrying a space-delimited
// "scope" claim, and the path the audit trail is written to.
func scopedDocsGateway(t *testing.T) (base, upstream, auditPath string, mint func(sub, scope string) string) {
	t.Helper()
	up, _ := newUpstreamServer(t, "read_doc", "edit_doc")
	iss := newFixtureIssuer(t)
	cfg := authedConfig(iss,
		[]config.Upstream{{ID: "docs", URL: up.URL, Namespace: "docs"}},
		&config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:       "readers",
				Subjects: &config.PolicySubjects{Scopes: []string{"docs:read"}},
				Allow:    []config.PolicyAllow{{Server: "docs", Names: []string{"read_doc"}}},
			}, {
				ID:       "writers",
				Subjects: &config.PolicySubjects{Scopes: []string{"docs:read", "docs:write"}},
				Allow:    []config.PolicyAllow{{Server: "docs", Names: []string{"edit_doc"}}},
			}},
		})
	auditPath = t.TempDir() + "/audit.jsonl"
	cfg.Audit = &config.Audit{Sinks: []config.AuditSink{{Type: "file", Path: auditPath}}}
	ts, _ := startGateway(t, cfg)
	mint = func(sub, scope string) string {
		return iss.mintClaims(t, jwt.MapClaims{
			"sub": sub, "aud": "https://gw.example.com", "scope": scope,
			"exp": time.Now().Add(time.Hour).Unix(),
		})
	}
	return ts.URL, up.URL, auditPath, mint
}

// A caller holding half of what a rule requires is refused, told exactly
// which half they lack — on the message and as structured data an agent can
// act on without parsing prose — and the record says the same thing.
func TestScopeDenialNamesTheMissingScope(t *testing.T) {
	base, _, auditPath, mint := scopedDocsGateway(t)
	session := connect(t, base, map[string]string{
		"Authorization": "Bearer " + mint("alice", "docs:read"),
	})

	_, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "docs__edit_doc"})
	if err == nil {
		t.Fatal("a caller holding one of two required scopes was allowed")
	}
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) {
		t.Fatalf("error = %v (%T), want a JSON-RPC error", err, err)
	}
	if wire.Code != codeDenied {
		t.Fatalf("code = %d, want %d", wire.Code, codeDenied)
	}
	if !strings.Contains(wire.Message, "requires scope docs:write") {
		t.Errorf("message = %q, want it to name the missing scope", wire.Message)
	}
	// Only the shortfall: naming "docs:read" too would invite a re-issued
	// token that dropped something the caller already holds.
	if strings.Contains(wire.Message, "docs:read") {
		t.Errorf("message = %q discloses a scope the caller already holds", wire.Message)
	}
	var data struct {
		MissingScopes []string `json:"missingScopes"`
	}
	if err := json.Unmarshal(wire.Data, &data); err != nil {
		t.Fatalf("error data %q: %v", wire.Data, err)
	}
	if strings.Join(data.MissingScopes, ",") != "docs:write" {
		t.Fatalf("data missingScopes = %v, want [docs:write]", data.MissingScopes)
	}

	// Audit is the single exit door, and a denial goes through it carrying
	// the same remedy — "not authorized" and "not authorized yet" are
	// different findings for whoever reads the trail.
	events := readAuditEvents(t, auditPath, "tools/call")
	var denials []audit.Event
	for _, e := range events {
		if e.Name == "docs__edit_doc" {
			denials = append(denials, e)
		}
	}
	if len(denials) != 1 {
		t.Fatalf("audited %d events for the denied call, want exactly 1: %+v", len(denials), denials)
	}
	if denials[0].Outcome != audit.OutcomeDenied {
		t.Errorf("outcome = %q, want %q", denials[0].Outcome, audit.OutcomeDenied)
	}
	if strings.Join(denials[0].MissingScopes, ",") != "docs:write" {
		t.Errorf("audited missingScopes = %v, want [docs:write]", denials[0].MissingScopes)
	}
}

// The enforcement pair, which is what makes the denial above more than a
// message: the tool the caller cannot invoke is also not in their list. The
// upstream is asked directly to show that the tool is there and that its
// absence is the gateway's doing rather than a fixture that never had it.
func TestScopeFilteringHidesAndDenies(t *testing.T) {
	base, upstream, _, mint := scopedDocsGateway(t)
	ctx := context.Background()

	direct := connect(t, upstream, nil)
	if got := strings.Join(toolNames(mustListTools(t, direct)), ","); got != "edit_doc,read_doc" {
		t.Fatalf("upstream lists %q, want both tools — the fixture must have what the gateway hides", got)
	}

	reader := connect(t, base, map[string]string{
		"Authorization": "Bearer " + mint("alice", "docs:read"),
	})
	if got := strings.Join(toolNames(mustListTools(t, reader)), ","); got != "docs__read_doc" {
		t.Fatalf("reader lists %q, want only docs__read_doc", got)
	}
	if _, err := reader.CallTool(ctx, &mcp.CallToolParams{Name: "docs__edit_doc"}); err == nil {
		t.Fatal("a hidden tool was still callable; invisibility alone is not enforcement")
	}
	// The half they do hold still works, so the gate is not simply closed.
	if _, err := reader.CallTool(ctx, &mcp.CallToolParams{Name: "docs__read_doc"}); err != nil {
		t.Fatalf("the granted tool was refused: %v", err)
	}
}

// Holding both scopes is the whole remedy: the same caller, one re-issued
// token later, sees and calls what they were refused.
func TestScopeGrantAdmitsOnceHeld(t *testing.T) {
	base, _, _, mint := scopedDocsGateway(t)
	session := connect(t, base, map[string]string{
		"Authorization": "Bearer " + mint("alice", "docs:read docs:write"),
	})

	if got := strings.Join(toolNames(mustListTools(t, session)), ","); got != "docs__edit_doc,docs__read_doc" {
		t.Fatalf("list = %q, want both tools", got)
	}
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "docs__edit_doc"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "echo:edit_doc" {
		t.Fatalf("result = %+v, want the upstream's own answer", res.Content[0])
	}
}

// The disclosure boundary, through the gateway: a caller refused for any
// reason other than scopes learns nothing about scopes. Telling them what a
// rule requires when they failed its group gate would name the credential
// guarding something they were never going to reach.
func TestDenialForAnotherReasonDisclosesNoScope(t *testing.T) {
	up, _ := newUpstreamServer(t, "run_payroll")
	iss := newFixtureIssuer(t)
	cfg := authedConfig(iss,
		[]config.Upstream{{ID: "payroll", URL: up.URL, Namespace: "payroll"}},
		&config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID: "finance-admins",
				Subjects: &config.PolicySubjects{
					Groups: []string{"finance"},
					Scopes: []string{"payroll:admin"},
				},
				Allow: []config.PolicyAllow{{Server: "payroll"}},
			}},
		})
	auditPath := t.TempDir() + "/audit.jsonl"
	cfg.Audit = &config.Audit{Sinks: []config.AuditSink{{Type: "file", Path: auditPath}}}
	ts, _ := startGateway(t, cfg)
	mint := func(sub string, groups []string, scope string) string {
		return iss.mintClaims(t, jwt.MapClaims{
			"sub": sub, "aud": "https://gw.example.com", "groups": groups, "scope": scope,
			"exp": time.Now().Add(time.Hour).Unix(),
		})
	}
	ctx := context.Background()

	// Wrong group, whether or not they hold the scope: nothing disclosed.
	for _, tc := range []struct {
		name   string
		groups []string
		scope  string
	}{
		{"wrong group, no scope", []string{"sales"}, "openid"},
		{"wrong group, holding the scope", []string{"sales"}, "payroll:admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := connect(t, ts.URL, map[string]string{
				"Authorization": "Bearer " + mint("mallory", tc.groups, tc.scope),
			})
			_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "payroll__run_payroll"})
			if err == nil {
				t.Fatal("an out-of-group caller was allowed")
			}
			var wire *jsonrpc.Error
			if !errors.As(err, &wire) {
				t.Fatalf("error = %v (%T), want a JSON-RPC error", err, err)
			}
			if strings.Contains(wire.Message, "scope") || len(wire.Data) > 0 {
				t.Fatalf("denial disclosed a scope requirement to an out-of-group caller: %q / %s", wire.Message, wire.Data)
			}
		})
	}

	// The caller the remedy is for: in the group, missing only the scope.
	session := connect(t, ts.URL, map[string]string{
		"Authorization": "Bearer " + mint("alice", []string{"finance"}, "openid"),
	})
	_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "payroll__run_payroll"})
	if err == nil {
		t.Fatal("an in-group caller without the scope was allowed")
	}
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) {
		t.Fatalf("error = %v (%T), want a JSON-RPC error", err, err)
	}
	if !strings.Contains(wire.Message, "requires scope payroll:admin") {
		t.Fatalf("message = %q, want it to name the missing scope", wire.Message)
	}

	// And the trail agrees on both readings: three denials, one of them
	// carrying the remedy.
	events := readAuditEvents(t, auditPath, "tools/call")
	var denied, withRemedy int
	for _, e := range events {
		if e.Outcome != audit.OutcomeDenied {
			continue
		}
		denied++
		if len(e.MissingScopes) > 0 {
			withRemedy++
			if e.Principal != "alice" {
				t.Errorf("a shortfall was recorded for principal %q, want only alice", e.Principal)
			}
		}
	}
	if denied != 3 || withRemedy != 1 {
		t.Fatalf("audited %d denials of which %d carried a shortfall, want 3 and 1: %+v", denied, withRemedy, events)
	}
}

func mustListTools(t *testing.T, s *mcp.ClientSession) *mcp.ListToolsResult {
	t.Helper()
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	return res
}
