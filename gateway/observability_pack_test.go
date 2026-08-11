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
	metricsSource = "metrics.go"
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
	for _, path := range []string{dashboardPath, rulesPath} {
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
