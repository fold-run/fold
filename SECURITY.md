# Security Policy

fold is a security boundary: it authenticates clients, enforces policy, and
brokers credentials to upstream MCP servers. Vulnerability reports are taken
seriously and handled with priority.

## Reporting a vulnerability

**Do not open a public issue for security vulnerabilities.**

Report privately via [GitHub private vulnerability reporting](https://github.com/fold-run/fold/security/advisories/new)
("Report a vulnerability" on the repository's Security tab).

If you cannot use that channel — no GitHub account, or the report concerns
GitHub's own availability — open an ordinary issue that says only that you
have a security report and how a maintainer can reach you privately. Put no
details in it; a maintainer will contact you on the channel you name.

Include what you can: affected version or commit, a minimal config and
reproduction, and the impact you believe it has. You can expect an
acknowledgment within 72 hours and a status update within 14 days. Please
allow a fix to be released before public disclosure; you will be credited in
the advisory unless you prefer otherwise.

## Disclosure

Fixes ship as a patch release on the supported line, with a GitHub Security
Advisory (and a CVE where one applies) published alongside. The advisory
names the affected versions, the fixed version, and any configuration that
mitigates the issue for deployments that cannot upgrade at once. We ask for
up to 90 days from acknowledgment before public disclosure and will normally
need far less; if a fix will take longer we will say so and agree a date.

## Scope

Reports of particular interest — anything that breaks the guarantees the
gateway exists to provide:

- Authentication bypass (JWT/JWKS verification, RFC 9728 metadata, the
  embedded ID-JAG token endpoint).
- Policy bypass: invoking a tool/prompt/resource a principal's policy denies,
  or seeing list entries policy should filter.
- Credential leakage: upstream API keys, exchanged tokens, or caller bearer
  tokens reaching a party that should not hold them (including via redirects,
  audit output, logs, or error responses).
- Cross-principal leakage through shared state (list caches, rate-limit or
  breaker state, bridged sessions).
- Audit evasion: producing a terminal response without an audit event.
- Request smuggling, DNS rebinding, or SSRF through the proxy path.

Out of scope: vulnerabilities in upstream MCP servers themselves, in the
official MCP SDKs (report those to their maintainers), or configurations
that disable documented protections (e.g. running without auth on an
exposed host).

## Supported versions

| Line | Security fixes | Until |
|---|---|---|
| The latest minor (`v1.N`) | Yes, as patch releases | Superseded by `v1.N+1` |
| The previous minor (`v1.N-1`) | Yes, for exploitable issues, as patch releases | 90 days after `v1.N` ships |
| Older `v1` minors | No | — |

Upgrades within a major version are drop-in under the compatibility contract
in the README's "API stability" section, which is what makes a short window
reasonable: moving from `v1.N-1` to `v1.N` changes no config field, endpoint,
error code, metric name, or audit shape. Security fixes land on `main` first;
the patch release follows for anything exploitable. When a `v2` line exists,
this table will name how long `v1` remains supported; there is no `v2`
planned.

## Dependencies

CI runs `govulncheck` on every merge (pinned in `go.mod` as a tool
dependency and bumped by Dependabot), a weekly drift workflow tests against
the latest MCP SDK and conformance suite and fuzzes every parser an untrusted
party controls, and dependency updates arrive via grouped weekly Dependabot
PRs. Every release artifact — binaries, images, the Helm chart, and the
checksum file — carries a sigstore attestation verifiable with
`gh attestation verify --owner fold-run`; see the deploy guide's "Verifying
what you deploy".
