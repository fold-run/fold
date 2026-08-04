# Security Policy

fold is a security boundary: it authenticates clients, enforces policy, and
brokers credentials to upstream MCP servers. Vulnerability reports are taken
seriously and handled with priority.

## Reporting a vulnerability

**Do not open a public issue for security vulnerabilities.**

Report privately via [GitHub private vulnerability reporting](https://github.com/fold-run/fold/security/advisories/new)
("Report a vulnerability" on the repository's Security tab).

Include what you can: affected version or commit, a minimal config and
reproduction, and the impact you believe it has. You can expect an
acknowledgment within 72 hours and a status update within 14 days. Please
allow a fix to be released before public disclosure; you will be credited in
the advisory unless you prefer otherwise.

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

While fold is pre-1.0, only the latest release receives security fixes.

From v1.0.0, the latest minor release line receives security fixes as patch
releases; older minors are expected to upgrade — upgrades within a major
version are drop-in under the compatibility contract in the README's "API
stability" section. In both eras the `main` branch is fixed first; a patch
release follows for anything exploitable.

## Dependencies

CI runs `govulncheck` on every merge, and a weekly drift workflow tests
against the latest MCP SDK and conformance suite. Dependency updates arrive
via grouped weekly Dependabot PRs.
