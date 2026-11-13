# Contributing

1. Read `docs/IMPLEMENTATION_BRIEF.md` (scope, contracts, milestones) and the ADRs in `docs/adr/`.
2. Public contracts live in `pkg/api/v1`, `schemas/`, MCP tool schemas and label names. Changing them requires an ADR.
3. Conventional Commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`). Small PRs, each with tests and docs.
4. Before pushing:
   ```sh
   go vet ./... && go test -race ./...
   go generate ./internal/config && git diff --exit-code schemas/
   golangci-lint run
   ```
5. Integration tests (containerized fake node, needs Docker): `DOL_INTEGRATION=1 go test ./test/integration/...`.
   Real-hardware end-to-end tests run only with `DOL_E2E=1` and must never touch resources without dol labels.
6. Error messages are product: say what happened, why, and what to do next.
