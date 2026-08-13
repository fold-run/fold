// Package doclint extracts the link graph from the repository's Markdown so a
// test can assert it resolves. It is test-support code, not API.
package doclint

import (
	"regexp"
	"strconv"
	"strings"
)

// Link is one inline Markdown link, split into the parts that can rot
// independently: the file it names and the heading anchor within it.
type Link struct {
	// Target is the raw path before any '#', empty for a same-file anchor.
	Target string
	// Fragment is the anchor without its '#', empty when the link has none.
	Fragment string
	// Line is the 1-indexed line it appeared on, so a failure can be clicked.
	Line int
}

var (
	// Inline links only. Reference-style links ("[text][ref]") are not used
	// anywhere in this repo; if they ever are, they will read as prose here
	// rather than being silently half-checked — which is why this does not
	// try to be a general Markdown parser.
	linkRE = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)

	headingRE = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*$`)

	// Markdown that carries no weight in a slug but would otherwise pollute
	// one: an inline link in a heading contributes its text, not its target.
	inlineLinkRE = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	htmlTagRE    = regexp.MustCompile(`<[^>]*>`)

	// GitHub's slugger keeps word characters, spaces, and hyphens; everything
	// else is dropped rather than replaced. That is why "`auth.ema`" anchors
	// as "authema" and not "auth-ema" — a distinction worth encoding, since
	// guessing it wrong is exactly the silent breakage this package exists to
	// catch.
	slugDropRE = regexp.MustCompile(`[^\w\s-]`)

	// Inline code spans in *prose* are not links: the launch notes carry
	// "`- [owner/repo](url)`" as a format template, which is a description of
	// a link rather than one. Headings are slugged before this is applied,
	// because there a code span contributes its text ("`upstreams`" anchors
	// as "upstreams").
	inlineCodeRE = regexp.MustCompile("`[^`]*`")
)

// Slug renders heading text the way GitHub anchors it.
func Slug(heading string) string {
	s := inlineLinkRE.ReplaceAllString(heading, "$1")
	s = htmlTagRE.ReplaceAllString(s, "")
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugDropRE.ReplaceAllString(s, "")
	// Each space becomes its own hyphen — runs are *not* collapsed. Dropping a
	// punctuation mark that sat between two words therefore leaves two spaces
	// and yields two hyphens: "The cardinality problem — settled" anchors as
	// "the-cardinality-problem--settled". Collapsing here would invent an
	// anchor GitHub never serves.
	return strings.ReplaceAll(strings.TrimSpace(s), " ", "-")
}

// Anchors returns every anchor the document defines, in the form GitHub would
// serve them. Repeated heading text disambiguates with a numeric suffix, the
// same way GitHub does, so two "### Notes" headings yield "notes" and
// "notes-1".
func Anchors(doc string) map[string]bool {
	out := map[string]bool{}
	seen := map[string]int{}
	for _, line := range codeFree(doc) {
		m := headingRE.FindStringSubmatch(line.text)
		if m == nil {
			continue
		}
		s := Slug(m[2])
		if s == "" {
			continue
		}
		if n := seen[s]; n > 0 {
			out[s+"-"+strconv.Itoa(n)] = true
		} else {
			out[s] = true
		}
		seen[s]++
	}
	return out
}

// Links returns every inline link in the document that points somewhere this
// repository controls. External schemes are skipped: a test must not depend on
// the network, and a dead vendor URL is not something a build should fail on.
func Links(doc string) []Link {
	var out []Link
	for _, line := range codeFree(doc) {
		text := inlineCodeRE.ReplaceAllString(line.text, "")
		for _, m := range linkRE.FindAllStringSubmatch(text, -1) {
			raw := m[1]
			if isExternal(raw) {
				continue
			}
			target, frag, _ := strings.Cut(raw, "#")
			out = append(out, Link{Target: target, Fragment: frag, Line: line.n})
		}
	}
	return out
}

func isExternal(s string) bool {
	for _, p := range []string{"http://", "https://", "mailto:", "tel:", "data:"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

type numberedLine struct {
	text string
	n    int
}

// codeFree drops fenced code blocks. A JSON example naming a URL is not a
// link, and a shell snippet is not a heading.
func codeFree(doc string) []numberedLine {
	var out []numberedLine
	fenced := false
	for i, l := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fenced = !fenced
			continue
		}
		if !fenced {
			out = append(out, numberedLine{text: l, n: i + 1})
		}
	}
	return out
}
