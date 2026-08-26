---
name: ship
description: Land a verified change in fold — branch, commit, PR title and body in the house register, the template checklist answered rather than ticked, then hand off to CI watching. Use when a change is ready to become a PR, when the user says ship it / open a PR / land this, or when work needs a branch before it grows further.
---

# Shipping a change in fold

`/preflight` decides whether a diff is ready. This lands it. `/watch-ci`
triages what CI says back. Every substantive change since #63 has merged
through a squashed PR — nothing but a release commit goes straight to main.

## 1. Gate before branching

Run `/preflight` first, and do not open a PR on an unverified diff. A red
PR costs a round trip; the gate costs a few minutes and runs locally.

## 2. Branch: `type/slug`

`feat/`, `fix/`, `docs/`, `chore/` — a short hyphenated slug after it.
`feat/downstream-keepalive`, `fix/console-sync-weekly-force-push`,
`docs/protocol-era-transition`.

The prefix survives even though the title will not have one. The branch
name is where the change gets classified; the title is where it gets
explained.

## 3. The title is a prose sentence naming the problem

This is the convention that is easiest to get wrong, because it is the
opposite of the usual advice and CONTRIBUTING said otherwise until it was
corrected.

**A PR title states the defect or the behavior, in a full sentence, with no
`area:` prefix.** Real ones:

- *A retry answering an upstream's question arrived looking like a first try*
- *Every federated list was announcing a cache scope that does not exist*
- *The weekly console bump has been force-pushing a branch it cannot see*
- *A client had to be refused before it could learn what to ask for*
- *Three things the gateway was letting past it*

Not `fix: forward MRTR continuation` — that is the branch name's job, and
it says what the diff does rather than what was wrong.

The test: a reader who knows fold should feel the problem land. A title
that describes the change instead of the defect is a changelog line.

**Commits that go straight to main keep the `area:` prefix** — `release:
v1.15.0`, `docs: repoint the conformance receipt at the v1.14.0 run`.
Those are housekeeping with no problem to name.

Merges are squashed, so GitHub appends `(#N)` and the title becomes the
commit subject verbatim. Write it to read well in `git log`.

## 4. The commit body is the rationale

Prose paragraphs, wrapped at ~76 columns, in this shape:

1. **What was wrong**, in enough detail that the reader does not need the
   diff to understand the stakes. Name what enforced the invariant before,
   if anything did.
2. **What the change is**, as bullets when there is more than one piece —
   each bullet leading with the mechanism and then the *why*, not a
   restatement of the file list.
3. **What fell out of the work** — the incidental findings, the portability
   fixes, the thing that turned out to be load-bearing. `git log 41c5757`
   is the model: it closes with the findings that deserved their own
   issues, "which is roughly the point."

Write it to a file and `git commit -F` — these bodies are long enough that
shell quoting will bite.

## 5. The PR body is written for a reviewer, not for the record

Four movements. The middle one is what makes the format worth keeping:

- **The claim**, in a paragraph or a table. What was unguarded, what is
  guarded now; what was broken, what works now.
- **Evidence, measured.** Numbers with the command that produced them —
  `keepAliveMs: 60` → **6 pings in 400 ms**; no `Server` section → **0**.
  Never "should now work."
- **## The part that needs review** — the section that earns the format.
  Say what you are least sure of, where you made a judgement call that
  could reasonably go the other way, and what you got wrong on the first
  attempt. PR #94 opens this section with "three consequences, two of which
  I initially got wrong," then measures all three. That is the standard.
- **## Verification** — what you actually ran, with results. Not the test
  plan you intend; the commands whose output you read.

`gh pr create --body-file` for the same quoting reason.

## 6. Answer the checklist, don't tick it

`.github/PULL_REQUEST_TEMPLATE.md` has six items and several will not apply
to a given change. Write *why* rather than ticking blindly — "Proxy-path
changes: none" is an answer; a checked box on a change with no proxy-path
diff is noise. If an item applies and you skipped it, say so and say why.

## 7. Push, open, watch

```bash
git push -u origin <branch>
gh pr create --base main --head <branch> --title "<prose>" --body-file <file>
gh run list -L 3          # match the head SHA
```

Then `/watch-ci`. Pushing and walking away is not done here.

## Merging

Squash merge, and **never without the user asking**. Branch protection has
`enforce_admins` off and requires no reviews, so nothing mechanical stands
between a green check and a merged PR — the restraint is the process.
