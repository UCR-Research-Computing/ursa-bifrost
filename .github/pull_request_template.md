## What and why

<!-- One or two sentences. Link the issue if there is one. -->

## Checklist

- [ ] `make check` passes
- [ ] New or changed safety guards have a line in `scripts/mutation_check.sh` and the check passes
- [ ] No new shell paths; new remote commands are constructors in `internal/backend` and in `TestAllowListIsClosed`
- [ ] User- or program-written text goes through `policy.Wrap`
- [ ] `docs/SPEC.md` and `CHANGELOG.md` updated if behaviour changed
- [ ] No real usernames, uids, internal IPs or secrets in fixtures or code
