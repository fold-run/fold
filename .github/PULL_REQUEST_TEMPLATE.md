## What & why

<!-- What does this change, and what problem does it solve? Link the issue if one exists. -->

## Checklist

- [ ] `make check` passes (fmt, tidy, vet, build, race tests, lint)
- [ ] New behavior is covered by integration tests with real SDK peers (see [CONTRIBUTING.md](../CONTRIBUTING.md))
- [ ] Proxy-path changes: `make bench` still passes the added-latency gate
- [ ] Wire-behavior changes: `make conformance` still passes 40/40
- [ ] README updated if this closes or widens a documented gap ("Not implemented"), adds config, or changes error codes
- [ ] Pin bumps (`scripts/conformance.sh`, MCP SDK) are in their own commit
