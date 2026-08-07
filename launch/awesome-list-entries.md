# Awesome-list PR entries (L−3 task)

Staged text for the awesome-list PRs. These take days to merge, so open them
early — the goal is that they land *during* launch week, not after it.

House rules from `launch-plan.md` §3 (L−2) and §7: **one line, no sales copy,
follow each list's existing format exactly.** An entry that reads like
marketing gets rejected or, worse, merged and resented.

## The description

One canonical line, reused everywhere (128 chars):

> Federates any number of upstream MCP servers into one governed endpoint, adding auth, policy, rate limiting, caching, and audit.

Deliberately *not* used here: "enterprise", "provably conformant", "40/40".
The lists are a directory, not a channel — the claim belongs in the README the
entry links to. Per §7, no numbers without instruments, and an awesome-list
entry carries no room for the receipt.

## Facts the entries assert

Verify these still hold before opening each PR:

| Claim | Source |
|---|---|
| Go codebase (🏎️) | single Go module |
| Self-hosted / local service (🏠) | you run the binary; no hosted plane |
| macOS + Linux (🍎 🐧) | `.goreleaser.yaml` builds `[linux, darwin] × [amd64, arm64]` |
| **No Windows** (🪟 omitted deliberately) | not a goreleaser target; the container is linux |

`fold` is a gateway/aggregator, not a leaf server — it belongs in an
aggregator/framework section, never in a domain category.

---

## 1. punkpeye/awesome-mcp-servers

- **Section:** `🔗 Aggregators`
- **Order:** alphabetical within the category — insert among the `f` entries.
- **Format:** `- [owner/repo](url) <emoji> - Description.`

```markdown
- [fold-run/fold](https://github.com/fold-run/fold) 🏎️ 🏠 🍎 🐧 - Federates any number of upstream MCP servers into one governed endpoint, adding auth, policy, rate limiting, caching, and audit.
```

Note: most entries in this list also carry a `glama.ai` score badge. That is
added by the list's own tooling, not by contributors — submit without it.

## 2. wong2/awesome-mcp-servers

- **Section:** `Frameworks` (the gateway/aggregator home in this list)
- **Format:** `**[Name](url)** - Description` — bold-linked name, plain name
  (not `owner/repo`), no emoji legend in this list.

```markdown
**[fold](https://github.com/fold-run/fold)** - Federates any number of upstream MCP servers into one governed endpoint, adding auth, policy, rate limiting, caching, and audit.
```

## 3. appcypher/awesome-mcp-servers

- **Section:** `🔗 Aggregators`
- **Format:** entries commonly prefix a favicon `<img>`; plain entries also
  exist. Check the immediate neighbors and match whichever dominates the
  section at PR time.

Plain variant:

```markdown
**[fold](https://github.com/fold-run/fold)** - Federates any number of upstream MCP servers into one governed endpoint, adding auth, policy, rate limiting, caching, and audit.
```

With icon (only if neighbors use one):

```markdown
<img height="12" width="12" src="https://fold.run/favicon.ico" alt="fold logo"> **[fold](https://github.com/fold-run/fold)** - Federates any number of upstream MCP servers into one governed endpoint, adding auth, policy, rate limiting, caching, and audit.
```

---

## Per-PR checklist

1. Read that repo's `CONTRIBUTING.md` at PR time — formats drift.
2. Confirm the section still exists under the same heading.
3. One entry, one line, one commit. No README restructuring, no fixing other
   people's entries in the same PR.
4. PR title: `Add fold to Aggregators` (or the section's actual name).
5. PR body: two sentences max — what fold is, and that it is Go, self-hosted,
   and open source (Apache-2.0). No links beyond the repo.
6. Do not mention the launch, and do not time the PR to it. These are
   directory entries, not announcements.

## Also worth doing (not an awesome list)

The official MCP registry is a higher-signal listing than any awesome list and
is not covered by `launch-plan.md`. Check whether `fold` should be registered
there as a gateway, and if so treat it as its own L−3 task with its own
verification pass — registry metadata is a published surface, so it falls under
the same "if it isn't in the repo, it isn't in the copy" rule.
