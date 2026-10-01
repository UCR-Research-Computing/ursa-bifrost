# ursa-bifrost

A read-only bridge between AI assistants and the Ursa Major Slurm cluster (UCR Research
Computing, Google Cloud). It is an MCP server and a CLI (`bifrost`) in one Go binary.

An assistant asks typed questions ("why did job 236 fail?", "what is billing right now?",
"which modules give me LAMMPS on GPU?", "check this batch script") and gets JSON back,
with the evidence and the exact scheduler commands that produced it. The server holds no
language model: it runs allow-listed scheduler queries and fixed diagnosis rules, and the
calling assistant does the reasoning.

Status: v0.3 (phases P1-P3 of `docs/SPEC.md`): SSH backend; read tiers R1/R2 and an opt-in act tier A1 (submit/cancel with two-step confirmation and caps).

## Install

```bash
scripts/install.sh          # builds ~/.local/bin/bifrost (Go 1.25+; toolchain auto-fetched)
# or: go install github.com/UCR-Research-Computing/ursa-bifrost/cmd/bifrost@latest
bifrost config init         # writes ~/.config/ursa-bifrost/config.yaml
bifrost doctor              # checks gcloud/SSH, the cluster user, the catalog, slurm --json
```

The default config reaches the login node the same way deep-research does:
`gcloud compute ssh ucrslurmcl-slurm-login-001 --tunnel-through-iap`, with one
multiplexed SSH connection reused for every call (~0.3 s per command once warm).

## CLI

```
bifrost status                    nodes powered up/down, queue, $/hour being spent now
bifrost partitions                partitions, cores, memory, GPUs, prices, what each is for
bifrost jobs [--state FAILED]     your jobs (queue + recent accounting)
bifrost job show 236              merged squeue + sacct record, efficiency, steps
bifrost job explain 236           deterministic diagnosis with evidence and a fix
bifrost job log 236 [--stderr]    redacted log tail
bifrost modules lammps            module search (versions, MPI prerequisite, GPU builds)
bifrost recipes pytorch           known-good recipes from the cluster catalog
bifrost check job.sbatch          static check of a batch script + worst-case cost
bifrost usage --since now-30days  node-hours, core-hours, efficiency, estimated cost
bifrost waste --since now-30days  avoidable spend: idle cores, idle nodes, repeat failures
bifrost health                    down/drained nodes, slow boots, long-pending jobs (R2)
bifrost ticket 236 [--text f.txt] reply draft for a "my job failed" ticket (R2; never sent)
bifrost results 265 [--download]  list / read / download a job's output folder
bifrost submit job.sh             plan, show worst-case cost, ask, submit (A1)
bifrost cancel|hold|release 265   two-step, own jobs only (A1)
```

### Submitting and getting results (A1)

Add `A1` to `tiers` in the config. Then:

```
$ bifrost submit hello.sh
Plan (nothing submitted yet):
  partition  standard, 1 node(s), time limit 0 h 05 min
  scheduler  would start at ... on ucrslurmcl-stdnodeset-0 (standard)
  worst case $0.12  (today so far $0.00 of $50.00 day cap)
Submit this job? [y/N] y
Submitted job 265 ...  folder on the cluster: ~/bifrost-jobs/<stamp>-bifrost-hello
$ bifrost job show 265
$ bifrost results 265 --download      # -> ~/ursa-results/265/
```

Each job runs in its own `~/bifrost-jobs/<stamp>-<name>` folder, so its results are
exactly that folder. Caps (config `caps:`): 4 nodes, 24 h, $25 per job, $50 per day by
default; a plan over a cap is refused before anything reaches the scheduler.

Every command takes `--json`. `bifrost check` exits 2 when the script has errors.

## MCP

Add to a client as a stdio server:

```json
{"mcpServers": {"ursa": {"command": "bifrost", "args": ["mcp"]}}}
```

Hermes:

```bash
hermes mcp add ursa --command bifrost --args mcp
hermes mcp test ursa
```

Claude Code: `claude mcp add ursa -- bifrost mcp`

Tools (R1): `cluster_status`, `partitions`, `jobs_list`, `job_show`, `job_explain`,
`job_log_tail`, `modules_search`, `module_show`, `recipes`, `script_check`, `my_usage`,
`waste_report`.
`job_results` (R1). Act tools (A1, only registered when `tiers` includes A1):
`job_submit` + `job_submit_confirm`, `job_cancel`/`job_hold`/`job_release` + `_confirm`.
The prepare tool returns a plan and a single-use `confirm_token`; the assistant must show
the plan and confirm only after you approve.
Staff tools (R2, only registered when `tiers` includes R2): `jobs_list_all`,
`job_show_any`, `job_explain_any`, `usage_report`, `waste_report_all`, `health`,
`ticket_draft`.
Resources: `hpc://catalog`, `hpc://policies`. Prompts: `diagnose_job`,
`write_batch_script`, `monthly_usage_summary`, `triage_ticket`.

## Safety model

- No shell passthrough. Commands can only be built by constructors in
  `internal/backend/command.go`, each validating its arguments; every argument is quoted.
  A test pins the allow-list (`squeue`, `sacct`, `sinfo`, `scontrol show`, `cat` of the
  catalog, `tail` of a job log, `module show`, `id -un`).
- Read-only by default. State changes exist only in tier A1, always two-step: a prepare
  call stores the exact action under a random single-use token (10-minute expiry, bound to
  the plan hash); the confirm call accepts nothing but the token. Caps are checked at
  prepare and the day cap again at confirm. Read tools carry `readOnlyHint: true`, act
  tools `false`.
- Log paths come from the job's accounting record, never from the caller, and must sit
  under `log_roots` for the job's owner (`/home/{user}/`, `/scratch/{user}/`).
- Text written by users or programs (log lines, scripts, submit lines) is returned only in
  `untrusted` blocks with a note telling the model it is data, not instructions.
- Secrets (tokens, keys, passwords, JWTs, private keys, credentials in URLs) are redacted
  from every output and from the audit log.
- Every call (CLI or MCP) appends a JSON line to `~/.local/share/ursa-bifrost/audit.jsonl`
  (mode 600): caller, client, tool, redacted args, decision, commands, size, duration.
- Short-TTL cache and a per-process rate limit keep scheduler load low; no background polling.
- Other users' jobs are denied unless the config grants tier R2.

## Development

```bash
make check      # gofmt, go vet, go test -race, build
```

Tests run against anonymized recordings of real Ursa Major output in `testdata/`
(Slurm 25.11.4, data_parser v0.0.44). `scripts/make_fixtures.py` regenerates them from a
read-only probe and refuses to write if a real username survives. `scripts/mcp_smoke.py`
drives `bifrost mcp` over stdio like a real client.

Design, phases and open questions: `docs/SPEC.md`.
