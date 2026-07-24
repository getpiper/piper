# AGENTS.md

Read `CLAUDE.md` in full first — it is the authoritative source for coding principles, commands, constraints, layering, and branch/PR/issue workflow. This file only captures high-signal facts an agent would otherwise get *wrong* that aren't obvious from filenames or `CLAUDE.md`.

- Single package: `go test ./internal/store/...`. Single test: `go test ./internal/store -run TestName`.
- Before claiming work is done, run the full CI sequence: `gofmt -l .` (it must print nothing), `go vet ./...`, `make test`, then `make cross`. `make test` and `make cross` alone do not catch formatting or vet failures.
- For unit tests of code that depends on `runtime.Runtime`, use `internal/runtime.FakeRuntime` from `internal/runtime/fake.go`. Real Docker is reserved for Docker integration and e2e tests, which must skip cleanly when Docker is unavailable.
- Runtime environment configuration belongs in `internal/config`; production packages must not call `os.Getenv` inline.
