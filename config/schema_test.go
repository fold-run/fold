package config

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaDefs maps each schema definition to the struct it must mirror; the
// root document is checked against Config itself.
var schemaDefs = map[string]reflect.Type{
	"upstream":       reflect.TypeOf(Upstream{}),
	"owner":          reflect.TypeOf(Owner{}),
	"upstreamAuth":   reflect.TypeOf(UpstreamAuth{}),
	"clientAuth":     reflect.TypeOf(ClientAuth{}),
	"timeouts":       reflect.TypeOf(Timeouts{}),
	"circuitBreaker": reflect.TypeOf(CircuitBreaker{}),
	"rateLimit":      reflect.TypeOf(RateLimit{}),
	"budget":         reflect.TypeOf(Budget{}),
	"tenant":         reflect.TypeOf(Tenant{}),
	"healthCheck":    reflect.TypeOf(HealthCheck{}),
	"auth":           reflect.TypeOf(Auth{}),
	"issuer":         reflect.TypeOf(Issuer{}),
	"ema":            reflect.TypeOf(EMAConfig{}),
	"policy":         reflect.TypeOf(Policy{}),
	"policyRule":     reflect.TypeOf(PolicyRule{}),
	"policySubjects": reflect.TypeOf(PolicySubjects{}),
	"policyAllow":    reflect.TypeOf(PolicyAllow{}),
	"audit":          reflect.TypeOf(Audit{}),
	"auditScrub":     reflect.TypeOf(AuditScrub{}),
	"auditSink":      reflect.TypeOf(AuditSink{}),
	"auditRetry":     reflect.TypeOf(AuditRetry{}),
	"routing":        reflect.TypeOf(Routing{}),
	"server":         reflect.TypeOf(ServerSection{}),
	"introspection":  reflect.TypeOf(Introspection{}),
	"console":        reflect.TypeOf(Console{}),
	"consoleOAuth":   reflect.TypeOf(ConsoleOAuth{}),
	"tracing":        reflect.TypeOf(Tracing{}),
	"discovery":      reflect.TypeOf(Discovery{}),
	"hook":           reflect.TypeOf(Hook{}),
	"serverIcons":    reflect.TypeOf(IconsSection{}),
	"identity":       reflect.TypeOf(Identity{}),
	"icon":           reflect.TypeOf(Icon{}),
}

func jsonFields(t reflect.Type) []string {
	var names []string
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func propertyNames(t *testing.T, node map[string]any) []string {
	t.Helper()
	props, ok := node["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema node has no properties object")
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// TestSchemaMatchesStructs is the drift guard: every json field on the
// config structs appears in the schema and vice versa, so the schema cannot
// silently fall behind the code (or advertise fields that do not exist).
func TestSchemaMatchesStructs(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(Schema(), &doc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}

	if got, want := propertyNames(t, doc), jsonFields(reflect.TypeOf(Config{})); !slices.Equal(got, want) {
		t.Errorf("root document: schema properties %v != struct fields %v", got, want)
	}

	defs, ok := doc["definitions"].(map[string]any)
	if !ok {
		t.Fatal("schema has no definitions")
	}
	for name, typ := range schemaDefs {
		def, ok := defs[name].(map[string]any)
		if !ok {
			t.Errorf("definition %q missing from schema", name)
			continue
		}
		if got, want := propertyNames(t, def), jsonFields(typ); !slices.Equal(got, want) {
			t.Errorf("definition %q: schema properties %v != struct fields %v", name, got, want)
		}
	}
	for name := range defs {
		if _, ok := schemaDefs[name]; !ok {
			t.Errorf("schema definition %q has no struct mapping in schemaDefs", name)
		}
	}
}

func compiledSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := jsonschema.UnmarshalJSON(bytes.NewReader(Schema()))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("fold.config.schema.json", raw); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile("fold.config.schema.json")
	if err != nil {
		t.Fatalf("schema does not compile: %v", err)
	}
	return schema
}

// TestSchemaAcceptsExampleConfig: the shipped example must validate against
// the shipped schema.
func TestSchemaAcceptsExampleConfig(t *testing.T) {
	schema := compiledSchema(t)
	data, err := os.ReadFile("../fold.config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(doc); err != nil {
		t.Errorf("example config rejected by schema: %v", err)
	}
}

// TestSchemaRejectsInvalidDocuments: the schema catches the structural
// errors an editor should flag before fold ever runs.
func TestSchemaRejectsInvalidDocuments(t *testing.T) {
	schema := compiledSchema(t)
	cases := map[string]string{
		"unknown top-level field": `{"upstreams":[{"id":"a","url":"http://x.test"}],"nope":true}`,
		"missing upstream id":     `{"upstreams":[{"url":"http://x.test"}]}`,
		"url and urls together":   `{"upstreams":[{"id":"a","url":"http://x.test","urls":["http://y.test"]}]}`,
		"neither url nor urls":    `{"upstreams":[{"id":"a"}]}`,
		"bad auth strategy":       `{"upstreams":[{"id":"a","url":"http://x.test","auth":{"strategy":"nope"}}]}`,
		"bad sink type":           `{"upstreams":[{"id":"a","url":"http://x.test"}],"audit":{"sinks":[{"type":"syslog"}]}}`,
		"claims object value":     `{"upstreams":[{"id":"a","url":"http://x.test"}],"policy":{"rules":[{"id":"r","subjects":{"claims":{"d":{}}},"allow":[{"server":"a"}]}]}}`,
		"uppercase upstream id":   `{"upstreams":[{"id":"Bad","url":"http://x.test"}]}`,
	}
	for name, raw := range cases {
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := schema.Validate(doc); err == nil {
			t.Errorf("%s: schema accepted an invalid document", name)
		}
	}
}
