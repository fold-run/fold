package gateway

import (
	"regexp"
	"testing"
)

// The deployable artifacts outside the chart — compose.yaml and the
// fold-discovery manifest — pin image tags by hand, and hands forget: compose
// sat at v1.11.0 for four minor releases while its comment said "bumped by the
// release flow" and the release skill said it pinned :latest and to leave it
// alone. Neither was true, and the alert rules compose mounts described error
// codes that gateway did not emit. The chart's appVersion is the one version
// the release workflow already refuses to publish wrong, so every other pin
// is held equal to it here.
func TestDeployPinsEqualChartAppVersion(t *testing.T) {
	chart := readFile(t, "../deploy/helm/fold/Chart.yaml")
	m := regexp.MustCompile(`(?m)^appVersion:\s*"?(v[0-9]+\.[0-9]+\.[0-9]+)"?`).FindStringSubmatch(chart)
	if m == nil {
		t.Fatal("Chart.yaml has no appVersion of the form vX.Y.Z")
	}
	want := m[1]

	pins := []struct{ path, image string }{
		{"../compose.yaml", "ghcr.io/fold-run/fold"},
		{"../compose.yaml", "ghcr.io/fold-run/fold-stdio"},
		{"../deploy/fold-discovery.yaml", "ghcr.io/fold-run/fold-discovery"},
	}
	for _, p := range pins {
		re := regexp.MustCompile(`image:\s*` + regexp.QuoteMeta(p.image) + `:(\S+)`)
		found := re.FindAllStringSubmatch(readFile(t, p.path), -1)
		if len(found) == 0 {
			t.Errorf("%s does not reference %s", p.path, p.image)
			continue
		}
		for _, f := range found {
			if f[1] != want {
				t.Errorf("%s pins %s:%s but the chart appVersion is %s — the release flow bumps them together (see .claude/skills/fold-release)", p.path, p.image, f[1], want)
			}
		}
	}
}
