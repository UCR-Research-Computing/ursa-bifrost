# CLAUDE.md

Guidance for AI coding agents working in this repo.

## What this is

`ursa-bifrost`: one Go binary (`bifrost`) that is both a CLI and an MCP server giving AI
assistants read-only, structured access to the Ursa Major Slurm cluster. Spec:
`docs/SPEC.md` (phases P0-P4, tool catalog, open questions Q1-Q15). Keep the spec's
status table and change log current when behaviour changes.

## Layout

- `cmd/bifrost/` CLI entry (`main.go`) and text renderers (`print.go`).
- `internal/backend/` the command allow-list (`command.go`), SSH backend (`ssh.go`),
  fixture backend for tests (`fixture.go`).
- `internal/slurm/` structs for Slurm 25.11 `--json` (data_parser v0.0.44).
- `internal/core/` typed operations; `service.go` has caching, the result envelope and
  `Call` (tier check, rate limit, audit) that every CLI command and MCP tool goes through.
- `internal/rules/` deterministic diagnosis rules for `job_explain`.
- `internal/policy/` redaction, untrusted wrapping, tiers, rate limit, audit log.
- `internal/mcpserver/` MCP tools/resources/prompts over core.

## Rules

- Gauntlet before every commit: `make check` (gofmt, vet, `go test -race`, build).
- Never add a tool or command that runs arbitrary shell. New remote commands are new
  constructors in `backend/command.go` with validated arguments, and must be added to
  `TestAllowListIsClosed`. State-changing commands are marked `write` and live only behind
  tier A1's two-step prepare/confirm flow (`internal/core/a1.go`); every new guard gets a
  line in `scripts/mutation_check.sh`.
- Every new tool goes through `core.Call` (so it is tier-checked and audited) and returns
  user/program-written text only via `policy.Wrap` (untrusted block).
- Every diagnosis rule needs a sample in `TestEachLogRuleFires`; prefer real failed-job
  logs as fixtures (anonymize with `scripts/make_fixtures.py`).
- Fixtures must not contain real usernames, uids or internal IPs; CI greps for it.
- Go toolchain: `GOTOOLCHAIN=go1.26.8` (system Go is 1.21; the Makefile sets it).
- Bump `internal/version` default and tag releases (`vX.Y.Z`); `scripts/install.sh`
  stamps the version from `git describe`.
- Live checks against the cluster are read-only (`bifrost doctor`, `status`, `jobs`,
  `job explain`). Never run compute on the login node.
