# Changelog

All notable changes. Versions follow [semver](https://semver.org); every release is a git
tag and a GitHub release. The full design history is in [docs/SPEC.md](docs/SPEC.md).

## v0.8.2 - 2026-10-02
- Docs and packaging only, no code change: README rewrite, `docs/CLIENTS.md` (Hermes,
  Claude Code, Codex, OpenCode, Gemini CLI, claude.ai, OpenAI API, scripts),
  `examples/mcp_client.py`, community files.
- Release binaries for Linux and macOS (amd64, arm64) with SHA256SUMS, and a one-line
  installer `scripts/get.sh`.

## v0.8.1 - 2026-10-02
- First day on shared nodes: the no-cores error reads as a sentence; `script_check`
  ignores module loads and run-time patterns inside comments. 116 mutation guards.

## agent-v0.3.0 - 2026-10-02
- ursa-agent 0.3.0: read-only cluster dashboard (pulse, jobs, usage, waste, storage,
  partitions, software, staged files; staff views).

## v0.8.0 - 2026-10-02
- Shared partitions: sharing read live from Slurm; on a shared partition a script must
  ask for its cores or `script_check` / `job_submit` refuse it; costs are the share of the
  node the job holds.

## v0.7.2 - 2026-10-02
- `module_show` loads the package's MPI first (hierarchical Lmod), new `mpi` argument;
  `ticket_draft` calls a completed job a success; `jobs_list` default page 50.

## v0.7.1 - 2026-10-02
- At most 8 concurrent SSH sessions per connection (login node MaxSessions is 10);
  refused channels retried on the same connection; transport errors reported as such.

## v0.7.0 - 2026-10-01
- Files in and out: staged uploads via signed links pulled by the job, `results_link`
  download links, paging for results, reads and logs with grep; helper tools
  `storage_usage`, `files_list`, `files_read`, `env_check`, `interactive_help`. No shell tool.

## v0.6.0 - 2026-10-01
- ursa-agent (ADK Gemini agent: chat page + A2A, approvals in code); `/whoami`.

## v0.5.0 - v0.5.4 - 2026-10-01
- Hosted server `bifrost serve`: MCP over HTTP, OAuth via Google sign-in, per-user
  identity, tiers and caps (v0.5.0); deployed on Cloud Run with `/health` (v0.5.1);
  access tokens survive restarts in a sealed store (v0.5.2); one-click repeat sign-in
  (v0.5.3); command-not-found diagnosis (v0.5.4).

## v0.4.0 - 2026-10-01
- Per-user IAP + OS Login backend (`backend: iap`).

## v0.3.0 - v0.3.3 - 2026-10-01
- Act tier A1: submit, cancel, hold, release with confirm tokens and caps; `job_results`
  (v0.3.0); fixes from the live test campaign, NO_VAL timestamps, MPI-aware
  `script_check` (v0.3.1-v0.3.3).

## v0.2.0 - v0.2.2 - 2026-10-01
- Staff tier R2: `waste_report`, `health`, `ticket_draft`; python-error diagnosis rule;
  `modules_search` returns containers and recipes.

## v0.1.0 - 2026-10-01
- First release: read-only MCP server and CLI for Ursa Major (phase P1).
