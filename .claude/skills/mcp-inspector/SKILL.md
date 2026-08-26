---
name: mcp-inspector
description: Drive the official MCP Inspector CLI against a locally running fold to see what a real client sees — namespacing, list merging, policy filtering, auth failures, ui:// resources. Use to reproduce a client-reported bug, to diff gateway behavior against an upstream directly, or to check a change by hand before writing the test.
---

# Inspecting fold as a real client

`make conformance` answers "does fold pass". The Inspector answers "what
does a client actually see", which is the question you have when a bug
report says a tool is missing and the tests are green.

Requires node/npx. Nothing to install:

```bash
npx @modelcontextprotocol/inspector --cli <url> --transport http --method tools/list
```

## Stand fold up

```bash
make build
FOLD_CONFIG=./fold.config.example.json ./fold --port 8080 --log-level debug
```

fold binds `127.0.0.1` by default and serves MCP at `/mcp`, so the
Inspector target is `http://127.0.0.1:8080/mcp`. `--validate` first if the
config is hand-edited. For a federation you can point at without real
upstreams, `scripts/conformance.sh` already stands up the everything-server
on `UPSTREAM_PORT` (3901) behind fold on `GATEWAY_PORT` (3902) — read it
for the fixture wiring rather than rebuilding it.

Auth: when `auth.mode` is `required`, pass the bearer yourself —
`--header "Authorization: Bearer $TOK"`. Exit code `3` means fold rejected
the token, `4` means it is not listening at all. Set `auth.mode` to
`disabled` in a scratch config when the thing under test is not auth.

## The recipes that earn their keep

**The invisibility diff** — the single most useful thing here, and the
thing the conformance suite only checks for its own fixture. Run the same
method against fold and against the upstream directly, and diff:

```bash
npx @modelcontextprotocol/inspector --cli http://127.0.0.1:3902/mcp --transport http \
  --method tools/list --format json | jq -S '.result' > /tmp/via-fold.json
npx @modelcontextprotocol/inspector --cli http://127.0.0.1:3901/mcp --transport http \
  --method tools/list --format json | jq -S '.result' > /tmp/direct.json
diff /tmp/direct.json /tmp/via-fold.json
```

In **passthrough** (single upstream, no namespace) that diff must be
empty. Anything in it is an invisibility-rule violation. With a namespace
configured, the only differences allowed are `{namespace}__{name}` on tool
and prompt names, merged lists, and per-principal policy filtering —
descriptions, schemas, `_meta`, and resource URIs must survive untouched.

**Namespacing round-trip** — the name a client sees must be the name it
can call:

```bash
npx @modelcontextprotocol/inspector --cli "$URL" --transport http \
  --method tools/list --format json | jq -r '.result.tools[].name'
npx @modelcontextprotocol/inspector --cli "$URL" --transport http \
  --method tools/call --tool-name 'gh__create_issue' --tool-arg title=probe
```

`-32043` (unknown namespace) on a name that `tools/list` just returned is
a routing bug. This is also how the MCP Apps gap in README "Not
implemented" reproduces: the bare, un-namespaced name answers `-32043`.

**Policy filtering is a pair** — invisibility plus call-denial. A tool
filtered out of `tools/list` for a principal must also refuse the call for
that principal. List with the token, then call the name anyway; `-32042`
is correct, a successful call is a policy hole:

```bash
npx @modelcontextprotocol/inspector --cli "$URL" --transport http \
  --header "Authorization: Bearer $LOW_PRIV" \
  --method tools/call --tool-name 'gh__delete_repo' --tool-args-json '{}'
```

**Resource URIs and `ui://`** — resource URIs are opaque and must come
back byte-identical, except MCP Apps `ui://` resources, which fold mints as
`ui://fold/{namespace}/{rest}`. Read one back to confirm it resolves:

```bash
npx @modelcontextprotocol/inspector --cli "$URL" --transport http --method resources/list --format json \
  | jq -r '.result.resources[].uri'
npx @modelcontextprotocol/inspector --cli "$URL" --transport http --method resources/read --uri 'ui://fold/gh/panel'
```

`--app-info` probes which tools ship a UI without calling them:
`--method tools/list --app-info` emits NDJSON, one line per tool.

## Reading failures

Exit codes are stable, so branch on them instead of scraping text: `0` ok,
`1` usage, `2` no app on the tool, `3` auth required, `4` unreachable, `5`
tool error or tool not found. Every non-zero exit also writes one JSON line
to stderr — `2>&1 | tail -1 | jq .error`.

Distinguish fold's minted errors from upstream errors: fold mints only the
codes registered in `gateway/upstream.go` and passes everything else
through verbatim. An upstream error arriving with a fold code means the
proxy path swallowed and replaced it.

## Then write the test

The Inspector is for finding and reproducing, not for proving. Anything it
turns up gets an integration test with a real SDK peer before the fix
lands — hand it to `integration-test-author`. A finding that only exists as
a shell transcript is not covered.
