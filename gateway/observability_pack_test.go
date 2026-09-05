package gateway

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The packaged dashboard and alert rules (deploy/helm/fold) are queries
// written against metric names this package declares. Nothing at deploy time
// checks that those names exist: a typo, or a metric renamed in some future
// refactor, yields a panel that draws nothing and an alert that never fires —
// the two failure modes an operator cannot see, because both look exactly like
// "healthy".
//
// So the pack is checked against the code, both directions:
//
//   - every fold_* name the pack references must be declared here, and
//   - every metric declared here must appear somewhere in the pack, so a new
//     metric forces the decision "does this belong on the dashboard" instead
//     of being quietly unshipped.
//
// This is the same lockstep discipline the config schema test applies, and it
// costs nothing at runtime. Metric names are frozen by the v1 contract
// (README, "API stability"), which is what makes publishing these queries safe
// in the first place.

const (
	dashboardPath = "../deploy/helm/fold/dashboards/fold-overview.json"
	rulesPath     = "../deploy/helm/fold/templates/prometheusrule.yaml"
	// The same alerts for deployments without the prometheus-operator: plain
	// Prometheus, which is what the compose observability profile runs.
	plainRulesPath = "../deploy/observability/alerts.yml"
	metricsSource  = "metrics.go"
)

// metricNamesUnexercised are declared metrics deliberately absent from the
// pack. Empty, and adding to it should require a reason: a metric worth
// exporting is usually worth a panel.
var metricNamesUnexercised = []string{}

var foldMetricRE = regexp.MustCompile(`fold_[a-z_]+`)

// seriesSuffixes are what a histogram exposes; queries name those, the code
// declares the base.
var seriesSuffixes = []string{"_bucket", "_sum", "_count"}

func baseMetric(name string) string {
	for _, s := range seriesSuffixes {
		if strings.HasSuffix(name, s) {
			return strings.TrimSuffix(name, s)
		}
	}
	return name
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// declaredMetrics reads the names out of the source that registers them,
// rather than scraping a live registry: a CounterVec with no observations
// exposes nothing, so a scrape would silently under-report exactly the
// metrics that have never fired.
func declaredMetrics(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, m := range regexp.MustCompile(`"(fold_[a-z_]+)"`).FindAllStringSubmatch(readFile(t, metricsSource), -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	if len(out) < 5 {
		t.Fatalf("only %d metric names found in %s — the extraction is wrong, not the pack", len(out), metricsSource)
	}
	slices.Sort(out)
	return out
}

func packReferences(t *testing.T) map[string][]string {
	t.Helper()
	refs := map[string][]string{}
	for _, path := range []string{dashboardPath, rulesPath, plainRulesPath} {
		for _, name := range foldMetricRE.FindAllString(readFile(t, path), -1) {
			base := baseMetric(name)
			if !slices.Contains(refs[base], path) {
				refs[base] = append(refs[base], path)
			}
		}
	}
	return refs
}

// A query naming a metric that does not exist draws an empty panel and arms an
// alert that can never fire.
func TestPackReferencesOnlyDeclaredMetrics(t *testing.T) {
	declared := declaredMetrics(t)
	for name, wheres := range packReferences(t) {
		if !slices.Contains(declared, name) {
			t.Errorf("%s references %q, which this package does not declare (see %s)",
				strings.Join(wheres, " and "), name, metricsSource)
		}
	}
}

// The other direction: a metric worth exporting is worth showing.
func TestEveryMetricAppearsInThePack(t *testing.T) {
	refs := packReferences(t)
	for _, name := range declaredMetrics(t) {
		if slices.Contains(metricNamesUnexercised, name) {
			continue
		}
		if _, ok := refs[name]; !ok {
			t.Errorf("%s is declared but appears in no dashboard panel or alert rule; "+
				"add one, or record why not in metricNamesUnexercised", name)
		}
	}
}

// The dashboard is delivered by templating the file into a ConfigMap, so it
// has to be a file Helm can embed and Grafana can parse — checked here rather
// than discovered when a sidecar rejects it in a cluster.
func TestDashboardIsWellFormed(t *testing.T) {
	raw := readFile(t, dashboardPath)
	// The `{{outcome}}` forms in here are Grafana legend placeholders, and they
	// are safe: the ConfigMap embeds the file with .Files.Get, which returns
	// contents rather than rendering them. Only `tpl` would interpret them, and
	// nothing does.
	var dash struct {
		UID    string `json:"uid"`
		Title  string `json:"title"`
		Panels []struct {
			Type    string `json:"type"`
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal([]byte(raw), &dash); err != nil {
		t.Fatalf("dashboard JSON does not parse: %v", err)
	}
	if dash.UID == "" || dash.Title == "" {
		t.Fatal("dashboard needs a stable uid and a title: the uid is what makes re-imports update rather than duplicate")
	}
	for _, p := range dash.Panels {
		if p.Type == "row" {
			continue
		}
		if len(p.Targets) == 0 {
			t.Errorf("panel %q has no query", p.Title)
		}
		for _, tgt := range p.Targets {
			if strings.TrimSpace(tgt.Expr) == "" {
				t.Errorf("panel %q has an empty expression", p.Title)
			}
		}
	}
}

// The alerts exist twice — as a PrometheusRule for the operator, and as a
// plain rules file for everything else — because the two consumers cannot read
// each other's format. Two copies drift, and the way they drift is silent: a
// deployment shape quietly missing an alert looks exactly like a deployment
// shape where nothing is wrong. So the alert names must match exactly.
func TestBothRuleFilesCarryTheSameAlerts(t *testing.T) {
	alertRE := regexp.MustCompile(`(?m)^\s*-?\s*alert:\s*(\w+)`)
	names := func(path string) []string {
		var out []string
		for _, m := range alertRE.FindAllStringSubmatch(readFile(t, path), -1) {
			out = append(out, m[1])
		}
		slices.Sort(out)
		return out
	}
	crd, plain := names(rulesPath), names(plainRulesPath)
	if len(crd) == 0 {
		t.Fatalf("no alerts found in %s — the extraction is wrong, not the rules", rulesPath)
	}
	if !slices.Equal(crd, plain) {
		t.Errorf("the two rule files disagree.\n  %s: %v\n  %s: %v\n"+
			"An alert added to one must be added to the other, or a deployment shape "+
			"silently loses it.", rulesPath, crd, plainRulesPath, plain)
	}
}

// The rule files must name only error codes fold actually mints. v1.15.0
// renumbered every minted code; the plain file was updated and the chart's
// copy was not, so an operator paged at 3am would have grepped the codebase
// for -32041 and found nothing. The name-lockstep test above could not see it
// — it compares alert names, and both files had the same names.
func TestPackNamesOnlyMintedErrorCodes(t *testing.T) {
	minted := map[string]bool{}
	for _, src := range []string{"upstream.go", "tasks.go"} {
		for _, m := range regexp.MustCompile(`(?m)^\s*code\w+\s*=\s*(-3\d{4})\b`).FindAllStringSubmatch(readFile(t, src), -1) {
			minted[m[1]] = true
		}
	}
	if len(minted) < 5 {
		t.Fatalf("found only %d minted codes in upstream.go/tasks.go — the extraction is wrong, not the pack", len(minted))
	}
	codeRE := regexp.MustCompile(`-3\d{4}\b`)
	for _, path := range []string{rulesPath, plainRulesPath, dashboardPath} {
		for _, code := range codeRE.FindAllString(readFile(t, path), -1) {
			if !minted[code] {
				t.Errorf("%s names error code %s, which fold does not mint (minted: %v)", path, code, minted)
			}
		}
	}
}

// packAlerts parses one rule file into alert → (expr, for, severity,
// summary), with the chart's templating normalized away so the two files can
// be compared field by field rather than name by name. Descriptions are
// exempt on purpose: the chart's copies legitimately refer to values keys the
// plain file has no equivalent for; what they say about error codes is
// covered by TestPackNamesOnlyMintedErrorCodes.
type packAlert struct{ expr, wait, severity, summary string }

func packAlerts(t *testing.T, path string) map[string]packAlert {
	t.Helper()
	raw := readFile(t, path)
	// Template normalization for the CRD file.
	raw = strings.ReplaceAll(raw, `{{ "{{" }}`, "{{")
	raw = strings.ReplaceAll(raw, `{{ "}}" }}`, "}}")
	raw = strings.ReplaceAll(raw, `{{ include "fold.fullname" . }}`, "fold")
	raw = regexp.MustCompile(`(?m)^\s*\{\{- with \$labels \}\}.*\n`).ReplaceAllString(raw, "")
	values := readFile(t, "../deploy/helm/fold/values.yaml")
	raw = regexp.MustCompile(`\{\{ \.Values\.metrics\.prometheusRule\.(\w+) \}\}`).ReplaceAllStringFunc(raw, func(ref string) string {
		key := regexp.MustCompile(`\.(\w+) \}\}`).FindStringSubmatch(ref)[1]
		m := regexp.MustCompile(`(?m)^\s+` + key + `:\s*(\S+)`).FindStringSubmatch(values)
		if m == nil {
			t.Fatalf("%s references .Values.metrics.prometheusRule.%s, which values.yaml does not set", path, key)
		}
		return m[1]
	})

	out := map[string]packAlert{}
	var cur string
	var a packAlert
	var inExpr bool
	var exprIndent int
	flush := func() {
		if cur != "" {
			out[cur] = a
		}
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for _, line := range strings.Split(raw, "\n") {
		trim := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if inExpr {
			if trim != "" && indent > exprIndent {
				a.expr = norm(a.expr + " " + trim)
				continue
			}
			inExpr = false
		}
		switch {
		case strings.HasPrefix(trim, "- alert:"):
			flush()
			cur = strings.TrimSpace(strings.TrimPrefix(trim, "- alert:"))
			a = packAlert{}
		case strings.HasPrefix(trim, "- name:"):
			flush()
			cur = ""
		case strings.HasPrefix(trim, "expr:"):
			v := strings.TrimSpace(strings.TrimPrefix(trim, "expr:"))
			if v == "|" || v == ">-" || v == "" {
				inExpr, exprIndent = true, indent
			} else {
				a.expr = norm(v)
			}
		case strings.HasPrefix(trim, "for:"):
			a.wait = strings.TrimSpace(strings.TrimPrefix(trim, "for:"))
		case strings.HasPrefix(trim, "severity:"):
			a.severity = strings.TrimSpace(strings.TrimPrefix(trim, "severity:"))
		case strings.HasPrefix(trim, "summary:"):
			a.summary = norm(strings.Trim(strings.TrimSpace(strings.TrimPrefix(trim, "summary:")), `"`))
		}
	}
	flush()
	return out
}

// Names matching is not enough: the same alert in both files must fire on the
// same expression, after the same duration, at the same severity, and say the
// same thing — or a deployment shape has a different alert wearing the same
// name. The CRD's thresholds are substituted from values.yaml, so this also
// pins that the chart defaults equal the plain file's literals.
func TestBothRuleFilesCarryTheSameExprsAndSummaries(t *testing.T) {
	crd, plain := packAlerts(t, rulesPath), packAlerts(t, plainRulesPath)
	if len(crd) < 10 {
		t.Fatalf("parsed only %d alerts from %s — the parser is wrong, not the rules", len(crd), rulesPath)
	}
	for name, c := range crd {
		p, ok := plain[name]
		if !ok {
			continue // the name test reports this
		}
		if c.expr != p.expr {
			t.Errorf("%s: expr differs\n  chart: %s\n  plain: %s", name, c.expr, p.expr)
		}
		if c.wait != p.wait {
			t.Errorf("%s: for differs (chart %q, plain %q)", name, c.wait, p.wait)
		}
		if c.severity != p.severity {
			t.Errorf("%s: severity differs (chart %q, plain %q)", name, c.severity, p.severity)
		}
		if c.summary != p.summary {
			t.Errorf("%s: summary differs\n  chart: %s\n  plain: %s", name, c.summary, p.summary)
		}
	}
}
