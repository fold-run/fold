package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// fold's list egress carries every field the SDK models forward for one
// reason: namespacedTools and its peers take a shallow struct copy (nt := *t)
// and overwrite the one or two fields fold deliberately rewrites. Nothing
// enumerates the rest, which is what makes it correct — a field fold has never
// heard of survives — and also what makes it fragile: a refactor to
// field-by-field construction, or a new field on one of the three sites that
// rebuild params instead of forwarding them, drops it with nothing red.
//
// That is not hypothetical. v1.15.0 fixed exactly this: tools/call,
// prompts/get, and the minted-ui:// branch of resources/read rebuilt their
// params and silently dropped inputResponses and requestState, so an upstream
// asked its question again on every retry, to the SDK's ten-attempt ceiling,
// re-prompting a human each pass. And icons — a field on five SDK types that
// fold has never mentioned anywhere — survive today by the same accident.
//
// So this file asserts the property directly: what a client sees through fold
// equals what a client sees hitting the upstream, except for a short declared
// list of differences. Widening that list is a code review.
//
// Technique. The comparison is over marshalled JSON rather than Go values,
// because the wire is the contract the invisibility rule is about, and because
// Tool.InputSchema is `any` — json.RawMessage on the server side,
// map[string]any on the client side — which no DeepEqual can reconcile.
// Reflection appears in one narrow role, assertFullyPopulated: a diff cannot
// notice a field *neither* side sets, and a field neither side sets is exactly
// the near-miss this file exists for.

// driftAllowlist names the JSON paths fold is permitted to change on the way
// out, per item kind. Everything else must arrive byte-identical.
type driftAllowlist map[string]string // path → why

var (
	allowTool = driftAllowlist{
		"name": "federation namespaces tool names: {namespace}{sep}{name}",
	}
	allowUITool = driftAllowlist{
		"name":                 "federation namespaces tool names",
		"_meta.ui.resourceUri": "MCP Apps interfaces are minted per namespace (uiresource.go)",
		"_meta.ui/resourceUri": "the extension's deprecated flat form, rewritten with the nested one",
	}
	allowPrompt = driftAllowlist{
		"name": "federation namespaces prompt names",
	}
	allowResource   = driftAllowlist{}
	allowUIResource = driftAllowlist{"uri": "ui:// is the single resource scheme fold rewrites"}
	allowTemplate   = driftAllowlist{}
	allowNothing    = driftAllowlist{}
)

const (
	driftUIURI    = "ui://widget/panel.html"
	driftModified = "2026-08-27T00:00:00Z"
)

// newDriftUpstream registers one of every listable item with every exported
// SDK field set non-zero, so a dropped field is visible as an absence.
func newDriftUpstream(t *testing.T, name string) *httptest.Server {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: name, Version: "1.0"}, nil)

	destructive, openWorld := true, true
	icons := []mcp.Icon{{
		Source:   "https://icons.example/" + name + ".png",
		MIMEType: "image/png",
		Sizes:    []string{"48x48", "96x96"},
		Theme:    "light",
	}}

	call := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	}
	tool := func(toolName string, meta mcp.Meta) *mcp.Tool {
		return &mcp.Tool{
			Meta:        meta,
			Name:        toolName,
			Title:       "Drift " + toolName,
			Description: "every field populated so a drop is visible",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
			OutputSchema: json.RawMessage(
				`{"type":"object","properties":{"n":{"type":"number"}},"required":["n"]}`),
			Annotations: &mcp.ToolAnnotations{
				Title:           "annotated",
				ReadOnlyHint:    true,
				IdempotentHint:  true,
				DestructiveHint: &destructive,
				OpenWorldHint:   &openWorld,
			},
			Icons: icons,
		}
	}
	s.AddTool(tool("plain", mcp.Meta{"com.example/vendor": "kept"}), call)
	s.AddTool(tool("app", mcp.Meta{
		"com.example/vendor": "kept",
		metaUI:               map[string]any{metaUIResourceURI: driftUIURI},
		metaUIResourceFlat:   driftUIURI,
	}), call)

	s.AddPrompt(&mcp.Prompt{
		Meta:        mcp.Meta{"com.example/vendor": "kept"},
		Name:        "brief",
		Title:       "Brief",
		Description: "every field populated",
		Arguments: []*mcp.PromptArgument{{
			Name: "topic", Title: "Topic", Description: "what about", Required: true,
		}},
		Icons: icons,
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: "hi"}},
		}}, nil
	})

	read := func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
			{URI: req.Params.URI, MIMEType: "text/plain", Text: "body"},
		}}, nil
	}
	resource := func(uri string) *mcp.Resource {
		return &mcp.Resource{
			Meta:        mcp.Meta{"com.example/vendor": "kept"},
			URI:         uri,
			Name:        "doc",
			Title:       "Doc",
			Description: "every field populated",
			MIMEType:    "text/plain",
			Size:        4,
			Annotations: &mcp.Annotations{
				Audience: []mcp.Role{"user"}, Priority: 0.5, LastModified: driftModified,
			},
			Icons: icons,
		}
	}
	s.AddResource(resource("file:///drift.txt"), read)
	s.AddResource(resource(driftUIURI), read)

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		Meta:        mcp.Meta{"com.example/vendor": "kept"},
		URITemplate: "file:///drift/{path}",
		Name:        "tmpl",
		Title:       "Template",
		Description: "every field populated",
		MIMEType:    "text/plain",
		Annotations: &mcp.Annotations{
			Audience: []mcp.Role{"assistant"}, Priority: 0.25, LastModified: driftModified,
		},
		Icons: icons,
	}, read)

	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	t.Cleanup(srv.Close)
	return srv
}

// TestEgressCarriesEverySDKField is the canary. It fails when a list egress
// path drops or alters a field outside the declared allowlist, and — via
// assertFullyPopulated — when the SDK grows a field this fixture does not set.
func TestEgressCarriesEverySDKField(t *testing.T) {
	up := newDriftUpstream(t, "drift")
	direct := connectDirect(t, up.URL)

	for _, tc := range []struct {
		name      string
		namespace string
		allow     func(kind string) driftAllowlist
	}{
		{"namespaced", "ns", func(kind string) driftAllowlist {
			switch kind {
			case "tool":
				return allowTool
			case "uitool":
				return allowUITool
			case "prompt":
				return allowPrompt
			case "resource":
				return allowResource
			case "uiresource":
				return allowUIResource
			default:
				return allowTemplate
			}
		}},
		// Passthrough rewrites nothing at all: the whole point of the mode is
		// that a client cannot tell fold is there.
		{"passthrough", "", func(string) driftAllowlist { return allowNothing }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := startGateway(t, &config.Config{
				Upstreams: []config.Upstream{{ID: "u", URL: up.URL, Namespace: tc.namespace}},
			})
			through := connect(t, ts.URL, nil)

			tools := listBy(t, direct, through, "tools")
			compareItem(t, "tools/list", tools["plain"], tools[public(tc.namespace, "plain")], tc.allow("tool"))
			compareItem(t, "tools/list", tools["app"], tools[public(tc.namespace, "app")], tc.allow("uitool"))

			prompts := listBy(t, direct, through, "prompts")
			compareItem(t, "prompts/list", prompts["brief"], prompts[public(tc.namespace, "brief")], tc.allow("prompt"))

			resources := listBy(t, direct, through, "resources")
			compareItem(t, "resources/list", resources["file:///drift.txt"],
				resources["file:///drift.txt#through"], tc.allow("resource"))
			compareItem(t, "resources/list", resources[driftUIURI],
				resources[mintedUIKey(tc.namespace)], tc.allow("uiresource"))

			templates := listBy(t, direct, through, "templates")
			compareItem(t, "resources/templates/list", templates["file:///drift/{path}"],
				templates["file:///drift/{path}#through"], tc.allow("template"))
		})
	}
}

// TestDriftFixturePopulatesEverySDKField is the half a diff cannot do: it
// fails when the SDK grows a field that neither the upstream nor the gateway
// sets, which is how icons could have been dropped with every other test green.
func TestDriftFixturePopulatesEverySDKField(t *testing.T) {
	up := newDriftUpstream(t, "drift")
	direct := connectDirect(t, up.URL)
	ctx := context.Background()

	tools, err := direct.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		assertFullyPopulated(t, "mcp.Tool", reflect.ValueOf(*tool))
	}
	prompts, err := direct.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range prompts.Prompts {
		assertFullyPopulated(t, "mcp.Prompt", reflect.ValueOf(*p))
	}
	resources, err := direct.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resources.Resources {
		assertFullyPopulated(t, "mcp.Resource", reflect.ValueOf(*r))
	}
	templates, err := direct.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tpl := range templates.ResourceTemplates {
		assertFullyPopulated(t, "mcp.ResourceTemplate", reflect.ValueOf(*tpl))
	}
}

// --- helpers -------------------------------------------------------------

func public(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "__" + name
}

func mintedUIKey(namespace string) string {
	if namespace == "" {
		return driftUIURI + "#through"
	}
	return uiScheme + uiMintHost + "/" + namespace + "/" + driftUIURI[len(uiScheme):] + "#through"
}

func connectDirect(t *testing.T, upstreamURL string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "direct", Version: "1.0"}, nil)
	session, err := client.Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: upstreamURL}, nil)
	if err != nil {
		t.Fatalf("connect direct: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// listBy collects both views into one map. Direct items are keyed by their own
// name or URI; gateway items by the public name, or by the URI with a
// "#through" marker where the URI is not rewritten and would collide.
func listBy(t *testing.T, direct, through *mcp.ClientSession, kind string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	add := func(key string, v any) {
		out[key] = toJSONObject(t, v)
	}
	ctx := context.Background()
	switch kind {
	case "tools":
		d, err := direct.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range d.Tools {
			add(i.Name, i)
		}
		g, err := through.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range g.Tools {
			add(i.Name, i)
		}
	case "prompts":
		d, err := direct.ListPrompts(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range d.Prompts {
			add(i.Name, i)
		}
		g, err := through.ListPrompts(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range g.Prompts {
			add(i.Name, i)
		}
	case "resources":
		d, err := direct.ListResources(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range d.Resources {
			add(i.URI, i)
		}
		g, err := through.ListResources(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range g.Resources {
			add(i.URI+"#through", i)
		}
	case "templates":
		d, err := direct.ListResourceTemplates(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range d.ResourceTemplates {
			add(i.URITemplate, i)
		}
		g, err := through.ListResourceTemplates(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range g.ResourceTemplates {
			add(i.URITemplate+"#through", i)
		}
	}
	return out
}

func toJSONObject(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// compareItem diffs the upstream's view against the gateway's, tolerating only
// the declared paths.
func compareItem(t *testing.T, method string, up, through map[string]any, allow driftAllowlist) {
	t.Helper()
	if up == nil {
		t.Fatalf("%s: fixture item missing from the direct view", method)
	}
	if through == nil {
		t.Fatalf("%s: item missing from the gateway's view entirely", method)
	}
	for _, d := range diffJSON("", up, through) {
		if _, ok := allow[d.path]; ok {
			continue
		}
		switch d.kind {
		case diffMissing:
			t.Errorf("%s dropped %q on the way out: present at the upstream (%v), absent through fold.\n"+
				"fold's egress paths carry unknown SDK fields only because they shallow-copy the struct "+
				"(upstream.go namespacedTools, \"nt := *t\"); a field-by-field rebuild drops whatever its "+
				"author had not heard of. If the change is intended, add %q to the allowlist in "+
				"egressdrift_test.go with a reason — that list is the record of what fold deliberately "+
				"rewrites, and today it is exactly {name, _meta.ui.resourceUri, ui:// uri}.",
				method, d.path, d.up, d.path)
		case diffChanged:
			t.Errorf("%s changed %q on the way out: upstream %v, through fold %v.\n"+
				"Only namespacing and ui:// minting may alter a listed item; everything else must arrive "+
				"as the upstream sent it (the invisibility rule, enforced on every merge by the "+
				"conformance suite). Add %q to the allowlist with a reason if this is deliberate.",
				method, d.path, d.up, d.through, d.path)
		case diffAdded:
			t.Errorf("%s added %q on the way out (%v). fold does not inject fields into an upstream's "+
				"definitions; if this is deliberate, declare it in the allowlist.",
				method, d.path, d.through)
		}
	}
}

type diffKind int

const (
	diffMissing diffKind = iota
	diffChanged
	diffAdded
)

type jsonDiff struct {
	path        string
	kind        diffKind
	up, through any
}

// diffJSON walks two decoded objects, recursing into nested objects so a
// difference reads as "_meta.ui.resourceUri" rather than "_meta".
func diffJSON(prefix string, up, through map[string]any) []jsonDiff {
	var out []jsonDiff
	keys := map[string]bool{}
	for k := range up {
		keys[k] = true
	}
	for k := range through {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)

	for _, k := range ordered {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		u, inUp := up[k]
		g, inThrough := through[k]
		switch {
		case inUp && !inThrough:
			out = append(out, jsonDiff{path: path, kind: diffMissing, up: u})
		case !inUp && inThrough:
			out = append(out, jsonDiff{path: path, kind: diffAdded, through: g})
		default:
			uo, uok := u.(map[string]any)
			go_, gok := g.(map[string]any)
			if uok && gok {
				out = append(out, diffJSON(path, uo, go_)...)
				continue
			}
			if !jsonEqual(u, g) {
				out = append(out, jsonDiff{path: path, kind: diffChanged, up: u, through: g})
			}
		}
	}
	return out
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// assertFullyPopulated walks a struct's exported fields and fails on any that
// is still zero, so a field the SDK adds and this fixture does not set becomes
// a test failure rather than a silence. It stops at `any`, interfaces, and
// maps, whose contents the fixture sets explicitly.
func assertFullyPopulated(t *testing.T, typeName string, v reflect.Value) {
	t.Helper()
	if v.Kind() != reflect.Struct {
		return
	}
	rt := v.Type()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := v.Field(i)
		name := typeName + "." + f.Name
		if fv.IsZero() {
			t.Errorf("%s is zero in the direct-from-upstream view. Either the SDK gained a field this "+
				"canary's fixture does not set — populate it in newDriftUpstream so a gateway that "+
				"drops it fails here — or the SDK stopped round-tripping it. This assertion is the "+
				"only thing standing between a field-by-field refactor of the egress paths and "+
				"silently dropping a field fold has never heard of; Icons is exactly that field.", name)
			continue
		}
		switch fv.Kind() {
		case reflect.Struct:
			assertFullyPopulated(t, name, fv)
		case reflect.Pointer:
			if fv.Elem().Kind() == reflect.Struct {
				assertFullyPopulated(t, name, fv.Elem())
			}
		case reflect.Slice:
			if fv.Len() > 0 && fv.Index(0).Kind() == reflect.Struct {
				assertFullyPopulated(t, fmt.Sprintf("%s[0]", name), fv.Index(0))
			}
		}
	}
}

var _ = strings.TrimSpace
