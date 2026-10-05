# Changelog

All notable changes. Versions follow [semver](https://semver.org); every release is a git
tag and a GitHub release. The full design history is in [docs/SPEC.md](docs/SPEC.md).

## Unreleased

## v0.9.14 - 2026-10-05
- Faster list reads (SPEC section 23). `jobs_list`, `my_usage`/`usage_report`,
  `waste_report(_all)` and `health`'s failure rate read accounting as plain text
  (`sacct -n -P --noconvert`, epoch times) instead of `sacct --json`. Measured on Ursa
  Major, one person over 7 days: 19.6 s and 12.6 MB before, 0.2 s after. A field-by-field
  comparison of 745 live jobs found the same users, states, times, TRES, exit codes and
  peak memory; CPU time is printed to the millisecond (to the second past an hour).
  `job_show`, `job_explain`, `ticket_draft` and `jobs_list job_ids` keep `--json`.
- `storage_usage` sizes folders 8 at a time within 40 s. Was one at a time within 100 s:
  on a busy home, 26 s with 3 of 91 folders unknown instead of 100 s with 43 unknown.
- `job_show include_script` explains a missing script in `script_note`. Ursa Major's
  `AccountingStoreFlags` does not include `job_script`, so Slurm keeps no scripts; it was
  never a cache problem.

## v0.9.13 - 2026-10-04
- The database-download warning in `script_check` reports the script's own line number
  (and sets the issue's `line` field). v0.9.12 counted lines after stripping comments and
  `#SBATCH`, so a download on line 7 read as line 2.

## v0.9.12 - 2026-10-04
- `script_check` knows the shared reference databases. A script that downloads one the
  cluster already hosts (`update_blastdb`, NCBI blast/db or nr FASTA, UniRef90/Swiss-Prot
  FASTA, `kraken2-build --standard`, `gtdbtk download-db`, `bakta_db download`,
  `download_eggnog_data.py`, `kaiju-makedb`, CheckM2/CheckV downloads, `busco --download`,
  Pfam-A, Rfam, `run_dbcan database`, the InterProScan tarball) gets a warning naming the
  `db-*` module, its variables, path and size. Only databases actually in the catalog
  count; comments are ignored; one warning per database.
- A job array running more than 8 tasks at once that scans a large database (`blast* -db
  core_nt|nr|nt`, `diamond blastp|blastx`) is warned that the tasks share /data/shared's
  read throughput (catalog `datasets.read_mib_per_s`, default 100 MiB/s), with each
  task's share.
- `modules_search` passes the catalog's `datasets.note`.

## v0.9.11 - 2026-10-04
- Shared reference data. Ursa Major has a 4 TiB read-only Filestore at /data/shared with
  public databases (BLAST core_nt/nr, DIAMOND nr, UniRef90, GTDB-Tk, Kraken2, Kaiju,
  Bakta, eggNOG, Pfam, PANTHER, BUSCO, InterProScan, ...), each with a `db-<name>`
  module. The catalog's new `datasets` section lists them; `modules_search` returns
  matches by name, module or variable (`db-` lists all) with `dataset_use`; the CLI prints
  them; `storage_usage` points at them; the server instructions tell assistants to load a
  `db-*` module instead of downloading; the disk-full and permission rules mention
  /data/shared.

## v0.9.10 - 2026-10-03
- `partitions` carries each partition's time limit and CPU instruction set (`cpu`: "AVX2
  only (e2)" on standard/spot/check, else "AVX-512"); the CLI table shows both.
- New log rule `illegal-instruction` (Illegal instruction, SIGILL, AVX-512 not
  supported); exit code 132 / signal 4 explain a binary built for a newer CPU, ahead of
  the generic crash rule.
- Deploy and install stamp the version from `v*` tags only (v0.9.9's hosted copy
  reported `agent-v0.4.2`, the agent tag on the same commit).

## agent-v0.4.2 - 2026-10-03
- ursa-agent's partition guidance follows the new cluster layout (standard/spot on e2,
  computehigh for tightly coupled MPI).

## v0.9.9 - 2026-10-03
- Checks that follow the new cluster layout: `script_check` refuses a `--time` over the
  partition's MaxTime (check's 15 min; Slurm would hold the job as PartitionTimeLimit),
  warns when code is compiled for AVX-512 or the build host (`-march=native`,
  `-mavx512*`, `-xHost`) on the AVX2-only e2 partitions, and notes that tightly coupled
  multi-node MPI on standard scales better on computehigh.
- `waste_report` low-CPU advice says ask for fewer cores, and points single-node
  computehigh work at standard.

## v0.9.8 - 2026-10-03
- Ursa Major standard and spot moved to e2-standard-32 in any us-central1 zone. Stockout
  alternatives now offer standard, then spot, then computehigh; segfault hints name the
  AVX2-only e2 nodes.

## v0.9.7 - 2026-10-03
- `jobs_list` with `job_ids` asks Slurm for just those jobs (`squeue -j` / `sacct -j`),
  uncached, and keeps only the caller's own rows. A program watching its jobs (the
  deep-research Lab) now sees a finished job on its next call instead of after the
  queue/accounting cache ran out: a 2-second check job on the always-on `check`
  partition took about 85 s to show as done, most of it cache.

## agent-v0.4.1 - 2026-10-03
- ursa-agent's instructions point people at the always-on `check` partition: env_check (a
  few-second job there) for "does this module load", and a short trial submit to `check`
  before the real partition.

## v0.9.6 - 2026-10-03
- env_check no longer runs on the login node. It runs its fixed template as a one-core,
  3-minute Slurm job (`srun -p check -c 1 -t 3 --immediate=120`) on the new always-on
  `check` partition and reports the job id and node it ran on. About 3 s on the warm node.
  The partition is config `env_check_partition` (default `check`).

## agent-v0.4.0 - 2026-10-03
- ursa-agent keeps sign-ins across restarts: each sign-in is also written, sealed, to the
  agent's own private bucket (`<project>-ursa-agent-data` at `/data`), so Cloud Run stopping
  the idle service no longer signs everyone out. Rotated refresh tokens are saved at once;
  sign-out and a refused refresh delete the record; records expire with the 12 h cookie.
  Chats and pending approvals still start fresh after a restart (SPEC 20.6).
- The bifrost OAuth client registration is kept as well (no new client per cold start).

## v0.9.5 - 2026-10-03
- IAP backend: the per-user OS Login key is extended in place (PATCH expiry) once less
  than 2 h is left, both on reconnect and in the background while a connection is in
  use, instead of being replaced by a new key. A new key needs the login node to pick
  it up (seconds, sometimes over a minute); an extended key works with no gap
  (measured live).
- A newly imported key is stored before its first login and is never deleted for being
  slow: callers wait for that same key (checking it is still on the profile) instead of
  importing another and restarting the wait. Fixes the 2026-10-03 outage (11:19-11:33
  UTC), where the overnight key lapsed and each replacement was deleted after ~40 s,
  failing every hosted call for about 15 minutes.
- The error while a new key propagates says so in plain words.
- ursa-agent 0.3.1: a panel whose bifrost call cannot reach the cluster gets HTTP 503
  `connecting` with a plain message and retries every 15 s (up to 8 times), keeping any
  data already shown, instead of a raw SSH error.
- 8 new mutation guards (v095), all killed.

## v0.9.4 - 2026-10-02
- `script_check` (and so `job_submit`): a redirection after `module load` is not a module.
  `module load apptainer 2>/dev/null || true` (the deep-research Lab harness) was reported
  as "module \"2>/dev/null\" not found", an error that blocks submission. Found by running
  `script_check` over 20 stored Lab scripts. 1 mutation guard.

## v0.9.3 - 2026-10-02
- `jobs_list` takes `job_ids` (up to 100): one call returns just those of the caller's
  own jobs, so a watcher (the deep-research Lab) polls every active run in one call
  instead of one `job_show` each. Ids are validated; it never widens the query beyond
  the caller's jobs.
- `jobs_list` rows carry `restarts` (Slurm's requeue count after node failures or
  preemption), from the live queue and from accounting; `job_show` takes the live
  count when it is higher.
- Program clients can have their own A1 caps (`max_cost_usd_per_day`, optional
  `max_cost_usd_per_job` and `max_submits_per_day`). A program with its own caps submits
  against its own ledger, so its spending and submission count never use up the
  person's, and the person's clients never use up the program's. Caps are bounded
  ($500/day, 1000 submits) and come as a set (a job cap or submit cap needs a day cap).
  `/whoami` shows `own_caps`. Built for `bifrost-deep-research`. 13 new mutation guards.

## v0.9.2 - 2026-10-02
- Deploy: min-instances 1 by default (`MIN_INSTANCES`, docs/DEPLOY.md): always warm, about $10 a month idle.
- Scaling phase 2 (docs/SCALING.md): at most 16 calls in flight per caller (a person,
  or a program for one person) on the hosted server; the 17th is refused at once with
  "16 of your calls are still running" and audited as `busy`. Accounting (sacct) runs
  at most 4 at once across everyone, since slurmdbd is shared; the rest wait their
  turn within the command timeout. 7 mutation guards.

## v0.9.1 - 2026-10-02
- Scaling phase 1 (docs/SCALING.md): output that is the same for everyone (node state,
  the whole queue, partitions, the site catalog) is cached once on the hosted server
  for all people instead of once per person, and identical commands in flight run once.
  Per-person output and writes are never shared. 5 new mutation guards.

## v0.9.0 - 2026-10-02
- Program clients: pre-registered OAuth clients in `users.yaml` (`clients:`) for programs
  such as Ultra. A program acts as the person who signed it in, with the person's tiers
  capped by the program's `tiers` ceiling (never a grant) and its own `calls_per_min`
  budget, separate from the person's chat clients. Program ids can't be registered or
  claimed; a disabled program is refused at sign-in, code exchange, refresh and on every
  request. Audit records read `program:<id>/<mcp client>`; `/whoami` shows the program,
  capped tiers and budget. One SSH connection per person, shared by their own clients
  and their programs. Files without `clients:` behave exactly as before.
- MIT License.

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
