package doclint

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The documentation is a graph of files pointing at each other's headings, and
// nothing else in this repository checks that the graph resolves: there is no
// link checker in CI, no Markdown lint, and the Stop hook is satisfied by any
// .md edit. So a moved section, a renamed heading, or a link written from the
// wrong directory is silent — the doc still renders, the link still looks like
// a link, and the reader lands on a page that no longer says what was promised.
//
// That is the same failure shape the observability pack test exists for: an
// artifact outside the compiler's reach, referencing names the code owns, with
// no signal when the two drift. This applies the same lockstep discipline to
// the docs, both halves:
//
//   - every relative link must name a file that exists, and
//   - every '#anchor' must name a heading that file actually defines.
//
// External URLs are deliberately not fetched. A test that needs the network is
// a test that fails for reasons unrelated to the change under review.

// repoRoot is this package's distance from the top of the module.
const repoRoot = "../.."

// skipDirs are trees whose Markdown is not ours to keep correct.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"testdata":     true,
}

func markdownFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".md") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", repoRoot, err)
	}
	if len(out) < 20 {
		// The repo has ~40 Markdown files. A walk that finds almost none means
		// the path or the filter broke, and a green test would be meaningless.
		t.Fatalf("found only %d Markdown files; the walk is wrong", len(out))
	}
	return out
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// rel renders a path the way someone would type it into a terminal, so a
// failure is copy-pasteable rather than littered with "../..".
func rel(path string) string {
	if r, err := filepath.Rel(repoRoot, path); err == nil {
		return r
	}
	return path
}

func TestMarkdownLinksResolve(t *testing.T) {
	// anchors are memoized per file: the docs cross-link heavily, and every
	// link into operations.md would otherwise re-parse it.
	anchors := map[string]map[string]bool{}
	anchorsFor := func(path string) map[string]bool {
		if a, ok := anchors[path]; ok {
			return a
		}
		a := Anchors(read(t, path))
		anchors[path] = a
		return a
	}

	var checked int
	for _, file := range markdownFiles(t) {
		for _, link := range Links(read(t, file)) {
			target := file
			if link.Target != "" {
				target = filepath.Join(filepath.Dir(file), link.Target)
				if _, err := os.Stat(target); err != nil {
					t.Errorf("%s:%d: link to %q — no such file", rel(file), link.Line, link.Target)
					continue
				}
			}
			checked++
			if link.Fragment == "" {
				continue
			}
			// Only Markdown carries headings; a '#' on anything else (a line
			// reference into source, say) is not ours to resolve.
			if !strings.EqualFold(filepath.Ext(target), ".md") {
				continue
			}
			if !anchorsFor(target)[link.Fragment] {
				t.Errorf("%s:%d: link to %q — %s defines no such heading",
					rel(file), link.Line, link.Target+"#"+link.Fragment, rel(target))
			}
		}
	}
	t.Logf("resolved %d internal links across the documentation", checked)
}

// TestSlug pins the rules that are easy to assume wrong. Each of these has a
// live counterpart in docs/configuration.md, and getting any of them backwards
// is how a plausible-looking anchor ends up pointing at nothing.
func TestSlug(t *testing.T) {
	for _, tc := range []struct{ heading, want string }{
		{"`upstreams`", "upstreams"},
		{"`auth.ema`", "authema"},                        // punctuation drops, never becomes a hyphen
		{"`upstreams` (required)", "upstreams-required"}, // parentheses drop, their spacing does not
		{"`auth` (gateway authentication)", "auth-gateway-authentication"},
		{"Deny wins, globally", "deny-wins-globally"},
		// Dropping the em dash leaves the spaces that flanked it, and each
		// becomes its own hyphen. docs/benchmarks.md links this exact anchor.
		{"The cardinality problem — settled", "the-cardinality-problem--settled"},
		{"Dashboards, alerts, and SLOs", "dashboards-alerts-and-slos"},
		{"What fold deliberately does not do", "what-fold-deliberately-does-not-do"},
		{"[Guides](#guides) above", "guides-above"}, // a link contributes its text
	} {
		if got := Slug(tc.heading); got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.heading, got, tc.want)
		}
	}
}

// TestAnchorsDisambiguate covers the case where two headings collide, which
// GitHub resolves with a numeric suffix rather than by serving the first.
func TestAnchorsDisambiguate(t *testing.T) {
	got := Anchors("## Notes\n\n## Notes\n\n## Notes\n")
	for _, want := range []string{"notes", "notes-1", "notes-2"} {
		if !got[want] {
			t.Errorf("missing anchor %q; got %v", want, got)
		}
	}
}

// TestFencedCodeIgnored keeps the checker from reading examples as claims: the
// config docs are full of JSON blocks, and CHANGELOG entries quote commands.
func TestFencedCodeIgnored(t *testing.T) {
	doc := "# Real\n\n```\n# Not A Heading\n[nope](does/not/exist.md)\n```\n\n[yes](README.md)\n"
	if a := Anchors(doc); a["not-a-heading"] {
		t.Error("heading inside a fenced block was indexed")
	}
	links := Links(doc)
	if len(links) != 1 || links[0].Target != "README.md" {
		t.Errorf("expected only the link outside the fence, got %+v", links)
	}
}

// TestInlineCodeIgnored covers documentation that describes a link rather than
// making one — the launch notes specify an awesome-list entry's format as
// "`- [owner/repo](url)`", where "url" is a placeholder, not a path.
func TestInlineCodeIgnored(t *testing.T) {
	doc := "Format: `- [owner/repo](url) - Description.` — see [README](README.md).\n"
	links := Links(doc)
	if len(links) != 1 || links[0].Target != "README.md" {
		t.Errorf("expected only the real link, got %+v", links)
	}
}

// A code span in a heading still contributes its text to the anchor, which is
// the opposite of how Links treats one. Both docs/configuration.md's headings
// and this repo's cross-links depend on that asymmetry holding.
func TestHeadingCodeSpansStillAnchor(t *testing.T) {
	if a := Anchors("## `tenants`\n"); !a["tenants"] {
		t.Errorf("code-span heading did not anchor as its text; got %v", a)
	}
}
