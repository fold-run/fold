#!/usr/bin/env bash
# Vendors the console assets from fold-run/fold-console into gateway/console.
#
# The assets are CHECKED IN, not fetched at build time. The Go module proxy is
# fold's distribution channel: `go run github.com/fold-run/fold/cmd/fold@latest`
# builds from the proxy zip alone, which runs no generators and — the reason a
# submodule is not an option — carries no submodule content. An unvendored tree
# would mean an empty gateway/console and a //go:embed build failure for every
# user installing that way.
#
# The pin is a commit SHA rather than a release tag or tarball. A git SHA is
# already a content hash: immutable, and impossible to re-point at different
# bytes. A tag can be moved and a release asset can be re-uploaded, which is
# why those need a separate checksum to be worth anything. This also keeps one
# pin idiom in the repo rather than two — see scripts/conformance.sh.
#
# Requires: git.
set -euo pipefail

# Bump deliberately, in its own commit. The scheduled console-sync workflow
# proposes bumps as PRs; it never merges them.
CONSOLE_REPO="${CONSOLE_REPO:-https://github.com/fold-run/fold-console.git}"
CONSOLE_COMMIT="${CONSOLE_COMMIT:-499253d0f70c895a76af60049910fb14dd4b80fd}"

# The exact file set that may enter the binary. //go:embed takes the whole
# directory, so without an allowlist the console repo's README, LICENSE, CI
# config, and dev harness would ship to every operator and be served under
# /console/. Adding a file to the shipped set is a reviewed change HERE, not a
# unilateral one upstream. gateway/introspection_test.go asserts the same set
# from the other side, against the embedded FS.
#
# fonts/OFL.txt is not decoration: OFL 1.1 requires the licence accompany the
# Font Software, and these subsets are embedded in every fold binary. It ships
# with them or the binary is out of compliance.
MANIFEST=(
  index.html
  app.js
  style.css
  fonts/IBMPlexSans-400-latin.woff2
  fonts/IBMPlexSans-600-latin.woff2
  fonts/GeistMono-400-latin.woff2
  fonts/GeistMono-600-latin.woff2
  fonts/OFL.txt
)

if [[ -z "$CONSOLE_COMMIT" ]]; then
  cat >&2 <<'EOF'
sync-console: CONSOLE_COMMIT is empty.

The pin above has a default, so reaching this means it was overridden with an
empty value. Pass a 40-hex commit from fold-run/fold-console:

  CONSOLE_COMMIT=<40-hex> make sync-console

and commit the result together with the updated default pin.
EOF
  exit 2
fi

root="$(cd "$(dirname "$0")/.." && pwd)"
# A fixed path under a world-writable /tmp (both vars unset: cron, some
# containers) could be pre-created by a co-tenant as a git repo whose config
# sets core.pager or core.sshCommand, which `git -C` would then honour. Git's
# safe.directory ownership check already refuses a differently-owned repo, and
# checking out by SHA makes content substitution impossible — so this is
# hygiene, not a hole. The cached path is still preferred when a private temp
# is available, because re-cloning on every run is slow.
if [[ -n "${RUNNER_TEMP:-}" ]]; then
  work="$RUNNER_TEMP/fold-console-sync"
  mkdir -p "$work"
elif [[ -n "${TMPDIR:-}" ]]; then
  work="$TMPDIR/fold-console-sync"
  mkdir -p "$work"
else
  work="$(mktemp -d)"
  trap 'rm -rf "$work"' EXIT
fi

echo "--- fetching fold-console @ ${CONSOLE_COMMIT:0:12}"
# The clone is cached in a temp directory, which the OS is free to reap:
# macOS deletes untouched files under /var/folders, and a partial sweep can
# leave a console/ directory whose .git has lost HEAD, config, and refs.
# Testing for the directory calls that a cache hit and hands git a shell of a
# repo ("fatal: not a git repository"), which fails every local run from then
# on until someone deletes the directory by hand. Ask git whether it is a
# repository instead, and re-clone when it says no. CI never sees this (a
# fresh RUNNER_TEMP each run), which is exactly why it has to be caught here.
if ! git -C "$work/console" rev-parse --git-dir >/dev/null 2>&1; then
  rm -rf "$work/console"
  git clone --quiet "$CONSOLE_REPO" "$work/console"
fi
git -C "$work/console" fetch --quiet origin "$CONSOLE_COMMIT"
git -C "$work/console" checkout --quiet "$CONSOLE_COMMIT"

src="$work/console/console"
if [[ ! -d "$src" ]]; then
  echo "sync-console: $CONSOLE_REPO@${CONSOLE_COMMIT:0:12} has no console/ directory" >&2
  exit 1
fi

# Fail before touching the working tree, so a bad pin cannot leave a partial
# vendor behind.
missing=()
for f in "${MANIFEST[@]}"; do
  [[ -f "$src/$f" ]] || missing+=("$f")
done
if (( ${#missing[@]} )); then
  echo "sync-console: pinned commit is missing manifest files:" >&2
  printf '  %s\n' "${missing[@]}" >&2
  exit 1
fi

echo "--- vendoring ${#MANIFEST[@]} files into gateway/console"
# Recreated rather than copied over: an upstream deletion must propagate, and
# `cp -r` onto the existing tree would leave the orphan in every binary built
# from here on.
rm -rf "$root/gateway/console"
for f in "${MANIFEST[@]}"; do
  mkdir -p "$root/gateway/console/$(dirname "$f")"
  cp "$src/$f" "$root/gateway/console/$f"
done

echo "--- recording the pin in gateway/console_source.go"
# Outside gateway/console/ on purpose: //go:embed console would sweep up a
# pin file placed in there and serve it at GET /console/SOURCE. As a Go file
# it also lands in `make fmt-check`, and it reaches /api/federation so a
# version-skew question has an answer without unpacking the binary.
cat > "$root/gateway/console_source.go" <<EOF
package gateway

// consoleSource is the fold-run/fold-console commit that gateway/console was
// vendored from. Generated by scripts/sync-console.sh — do not edit by hand.
//
// It lives here rather than in gateway/console/ because \`//go:embed console\`
// would sweep up a file in that directory and serve it at GET /console/SOURCE.
// As a Go file it is also covered by \`make fmt-check\`, and it reaches
// /api/federation so a version-skew question has an answer without unpacking
// the binary.
const consoleSource = "$CONSOLE_COMMIT"
EOF

echo "synced fold-console @ ${CONSOLE_COMMIT:0:12}"
