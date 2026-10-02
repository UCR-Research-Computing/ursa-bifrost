# Security policy

bifrost gives AI assistants access to a shared research cluster, so we take reports
seriously and answer quickly.

## Reporting a vulnerability

Please **do not open a public issue**. Use GitHub's private reporting:
[Report a vulnerability](https://github.com/UCR-Research-Computing/ursa-bifrost/security/advisories/new).
If that is not possible, email the maintainers through
[UCR Research Computing](https://github.com/UCR-Research-Computing).

Include what you did, what happened, and what you expected. We aim to acknowledge within
two working days and to ship a fix for confirmed issues within a week.

## What is in scope

- Running any command on the cluster that is not one of the allow-listed constructors
  (`internal/backend/command.go`, `files.go`): shell injection, argument smuggling.
- Reading another user's jobs, logs or files without tier R2, or reading hidden or
  credential files.
- Acting (submit, cancel, hold, release, upload) without the two-step confirm, or beyond
  the caps.
- Bypassing sign-in, the ucr.edu restriction, `users.yaml`, token rotation or expiry on
  the hosted server.
- Secrets leaking through tool output or the audit log.
- Prompt injection that makes the server, not the model, do something it should not.

## Design notes for reviewers

- No tool runs arbitrary shell; `TestAllowListIsClosed` pins the command list.
- `scripts/mutation_check.sh` disables each of 116 guards in turn and requires a test
  to fail.
- Text from users and programs is returned in `untrusted` blocks.
- The hosted server is deployed `--allow-unauthenticated` on Cloud Run: all access
  control is in `internal/server` (OAuth 2.1, PKCE, Google ID token checks, users file
  re-read per request). See [docs/CLOUD_PLAN.md](docs/CLOUD_PLAN.md).

## Supported versions

Only the latest release is supported. The hosted server always runs the latest tag.
