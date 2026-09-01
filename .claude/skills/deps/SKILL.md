---
name: deps
description: Triage a govulncheck failure, review a dependabot PR, or bump a dependency in fold — including the case where CI goes red with no commit behind it. Use when make vuln fails, when CI's vuln job is red, when a dependabot PR needs review, or when bumping the MCP SDK or the Go toolchain.
---

# Dependencies and vulnerabilities

fold has 13 direct dependencies and runs at a trust boundary, so the supply
chain is part of the threat model rather than housekeeping. Four separate
mechanisms watch it, and knowing which one is talking saves the whole
investigation.

| Mechanism | Watches | Produces |
| --- | --- | --- |
| CI `vuln` job / `make vuln` | Go advisories against this tree | A red merge gate |
| dependabot (weekly ×3) | gomod, github-actions, docker base images | PRs |
| `drift.yml` (Mondays 06:00 UTC) | the **latest** MCP SDK and conformance suite | An issue |
| `console-sync.yml` (on demand; bumped at release) | the vendored console | A PR — see `/console-sync` |

## A red govulncheck often has no commit behind it

**Check this first, before reading the diff.** `make vuln` runs
`govulncheck@latest` against the *live* advisory database, so a run that
was green yesterday goes red today because an advisory was published — not
because anything changed here.

```bash
go version                          # the toolchain in use
grep '^go ' go.mod                  # the version this module requires
make vuln                           # read WHICH module and WHICH advisory
```

If the finding is in the **standard library**, the fix is a Go patch
version, not a dependency bump: raise the `go` line in `go.mod` and let the
toolchain download it. That has happened here before — six standard-library
advisories at once, fixed by moving to a Go patch release.

If the finding is in a dependency, bump that dependency alone. Do not run a
blanket `go get -u`; a vulnerability fix and a feature bump should not
arrive in one commit.

Either way the image builder base may also need repinning — it sets
`GOTOOLCHAIN=local`, so an image whose base is below what `go.mod` requires
**cannot fix it at build time and fails the build**. That skew once sat on
main through two green merges and failed after the tag was pushed. CI's
`image` job now catches it per-PR; check `deploy/docker/*.Dockerfile` when
you move the Go line.

## Reviewing a dependabot PR

Three ecosystems, three different review questions.

**gomod.** Minor/patch are grouped into one PR, majors come separately. For
the grouped PR, read the go.sum diff for anything that is not the stated
bump — a new transitive dependency is a finding, not a formality. Then
`make check`. The MCP SDK is the one that moves fast and the one that can
change wire behavior: an SDK bump gets `make conformance` and, across a
protocol version, `/spec-drift`.

**github-actions.** `uses:` lines are pinned to **commit SHAs**, not tags,
because a moved tag on a dependency action is code execution inside jobs
that hold `id-token: write` and `attestations: write`. Verify the new SHA
belongs to the version in the trailing comment; the comment is
documentation and can drift from the pin. A PR that swaps a SHA for a tag
is a downgrade in posture regardless of what it claims to fix.

**docker.** Base images are pinned by digest for the same reason. Same
check: the digest is the pin, the tag comment is a note.

## Bumping deliberately

Both upstream pins are commit SHAs — a SHA is a content hash, so it cannot
be re-pointed at different bytes:

- `scripts/conformance.sh` → `/conformance`
- `scripts/sync-console.sh` → `/console-sync`

**Each pin bump goes in its own commit** (CONTRIBUTING, "Pinned
upstreams"), so a bisect can land on it.

## When drift.yml opens an issue

It failed against the *latest* SDK or conformance suite, not the pinned
ones, so nothing is broken for users yet. Two jobs, two meanings:

- **sdk-latest** — build or tests broke against the newest
  `modelcontextprotocol/go-sdk`.
- **conformance-latest** — a scenario regressed against the newest suite.

The fix is a judgement call about what fold should do, which is why it
opens an issue rather than a PR. Start at `/spec-drift` when it is the
protocol that moved, `/conformance` when it is the suite.

## Before done

```bash
make vuln
make check
make conformance     # any SDK bump
```
