# Contributing

Thanks for helping. bifrost is small on purpose and strict about safety, so a few rules
matter more than usual.

## Setup

```bash
git clone https://github.com/UCR-Research-Computing/ursa-bifrost
cd ursa-bifrost
make check          # gofmt, go vet, go test -race, build (Go toolchain auto-fetched)
```

Tests run on recorded, anonymized Slurm output in `testdata/`; you don't need cluster
access to contribute.

## Workflow

1. Branch from `main`, one change per pull request.
2. Run `make check` before every commit. CI runs the same plus a fixture/secret scan.
3. If you touched a safety guard, run `scripts/mutation_check.sh` and add a line for any
   new guard: every guard must be caught by a test.
4. Update `docs/SPEC.md` (status table, change log) when behaviour changes, and
   `CHANGELOG.md` for anything a user would notice.
5. Releases are tags (`vX.Y.Z`) cut by maintainers after merge; the release workflow
   builds the binaries.

## Rules that are not negotiable

- **No shell.** Never add a tool or command that runs arbitrary shell. A new remote
  command is a new constructor in `internal/backend/command.go` with validated arguments,
  and goes into `TestAllowListIsClosed`.
- **Acting is two-step.** Anything that changes state is marked `write`, lives in tier A1,
  and goes through the prepare/confirm flow in `internal/core/a1.go`.
- **Everything goes through `core.Call`** (tier check, rate limit, audit), and
  user- or program-written text is returned only through `policy.Wrap`.
- **Diagnosis rules need a real sample** in `TestEachLogRuleFires`; anonymize logs with
  `scripts/make_fixtures.py`.
- **No real usernames, uids, internal IPs or secrets** in fixtures or code; CI checks.

## Reporting bugs and ideas

Open an issue with the form that fits. Security problems: see [SECURITY.md](SECURITY.md).
