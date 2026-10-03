<div align="center">

# ursa-bifrost

**Let any AI assistant work a Slurm cluster, safely.**

One Go binary that is both an MCP server and a CLI. It gives Claude, Codex, Gemini,
Hermes, OpenCode and your own scripts typed, audited, read-mostly access to the Ursa
Major HPC cluster at UC Riverside: job status, failure diagnosis, costs, software
modules, batch-script checks, results, and (opt-in, two-step) submit and cancel.

[![CI](https://github.com/UCR-Research-Computing/ursa-bifrost/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/UCR-Research-Computing/ursa-bifrost/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/UCR-Research-Computing/ursa-bifrost?sort=semver)](https://github.com/UCR-Research-Computing/ursa-bifrost/releases)
[![Go version](https://img.shields.io/github/go-mod/go-version/UCR-Research-Computing/ursa-bifrost)](go.mod)
[![MCP](https://img.shields.io/badge/MCP-Streamable%20HTTP%20%2B%20stdio-blue)](https://modelcontextprotocol.io)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

[Quick start](#quick-start) ·
[All clients](docs/CLIENTS.md) ·
[What you can ask](#what-you-can-ask) ·
[Tools](#tools) ·
[Safety](#safety-model) ·
[Self-host](docs/DEPLOY.md) ·
[Spec](docs/SPEC.md)

</div>

---

```text
You:      Why did job 236 fail?
Claude:   [job_explain 236]
          It failed after one minute on gpul4: the log says
          "ModuleNotFoundError: No module named 'pandas'". The script calls python
          without an environment. Load python-ml (PyTorch/GPU) or python-sci (CPU)
          before running it, or create a uv/Pixi environment inside the job.
```

That diagnosis came from the live cluster. The server holds no language model: it runs
allow-listed scheduler queries and fixed diagnosis rules, returns JSON with the evidence
and the exact commands it ran, and the assistant does the reasoning. Nothing a model
sends is ever run as a shell command.

## Quick start

### Use the hosted server (nothing to install)

With a UCR account that Research Computing has added, point your client at

```text
https://bifrost-mcp-125853442225.us-central1.run.app/mcp
```

and sign in with Google when it asks. One line for each common client:

| Client | Install |
|---|---|
| **Hermes Agent** | `hermes mcp add ursa --url https://bifrost-mcp-125853442225.us-central1.run.app/mcp --auth oauth` |
| **Claude Code** | `claude mcp add --transport http ursa https://bifrost-mcp-125853442225.us-central1.run.app/mcp`, then `/mcp` -> Authenticate |
| **Codex CLI** | `codex mcp add ursa --url https://bifrost-mcp-125853442225.us-central1.run.app/mcp` (signs in right away) |
| **OpenCode** | `opencode mcp add ursa --url https://bifrost-mcp-125853442225.us-central1.run.app/mcp`, then `opencode mcp auth ursa` |
| **Gemini CLI** | `gemini mcp add -s user -t http ursa https://bifrost-mcp-125853442225.us-central1.run.app/mcp`, then `/mcp auth ursa` |
| **claude.ai / Claude Desktop** | Settings -> Connectors -> Add custom connector, paste the URL |
| **OpenAI API** | a `{"type": "mcp", "server_url": ...}` tool in the Responses API ([example](docs/CLIENTS.md#openai-api-responses-api)) |
| **Python / curl** | [`examples/mcp_client.py`](examples/mcp_client.py): `python3 examples/mcp_client.py login`, standard library only |

Config-file versions, local (stdio) variants and troubleshooting for every client:
**[docs/CLIENTS.md](docs/CLIENTS.md)**.

### Or run it locally (your own gcloud login, no server)

```bash
curl -fsSL https://raw.githubusercontent.com/UCR-Research-Computing/ursa-bifrost/main/scripts/get.sh | sh
# Linux/macOS release binary to ~/.local/bin, checksum-verified. Or, with Go:
go install github.com/UCR-Research-Computing/ursa-bifrost/cmd/bifrost@latest
# or from a clone: scripts/install.sh   (stamps the version; Go toolchain auto-fetched)

bifrost config init     # writes ~/.config/ursa-bifrost/config.yaml
bifrost doctor          # checks gcloud, SSH to the login node, the catalog
bifrost status
```

Then add it to a client as a stdio server: `claude mcp add ursa -- bifrost mcp`,
`hermes mcp add ursa --command bifrost --args mcp`, `codex mcp add ursa -- bifrost mcp`,
or in any JSON config:

```json
{ "mcpServers": { "ursa": { "command": "bifrost", "args": ["mcp"] } } }
```

Local mode reaches the login node the same way you would by hand
(`gcloud compute ssh ucrslurmcl-slurm-login-001 --tunnel-through-iap`), with one
multiplexed SSH connection reused for every call: about 0.3 s per command once warm.

## What you can ask

- "What's running on the cluster and what is it costing per hour?"
- "Why did my job 236 fail?"
- "Which modules give me LAMMPS with GPU support?"
- "Check this batch script before I submit it."
- "How much did I spend last month, and how much was wasted on idle cores?"
- "Write a script for PyTorch on one L4 GPU for 4 hours, show me the cost, then submit it."
- "Show the last 200 lines of job 265's stderr, then give me download links for its results."
- "Give me the exact command for an interactive session with 8 cores and a GPU."
- Staff: "Which nodes are down?" / "Draft a reply to this ticket about job 236."

## The CLI

Everything the MCP tools do is also a command, and every command takes `--json`.

```text
$ bifrost status
ursa-major (Slurm 25.11.4): 0 running, 0 pending, 0 node(s) powered up, now $0.00/hour

PARTITION    NODES  UP  ALLOC  IDLE-UP  BOOT  DOWN  RUN  PEND  $/NODE-H  $/H NOW
check        4      1   0      1        0     0     0    0     $0.13     $0.13
computehigh  4      0   0      0        0     0     0    0     $1.87     $0.00
gpul4        8      0   0      0        0     0     0    0     $1.15     $0.00
highmem      8      0   0      0        0     0     0    0     $4.19     $0.00
standard*    48     0   0      0        0     0     0    0     $1.07     $0.00
...

$ bifrost check train.sbatch
PROBLEMS FOUND  partition standard  worst-case cost $0.13  (1 cores, 6% of a node)
  error   line 4: module "python3" not found in the cluster catalog; closest: pythia8, python-ml
  error   standard shares nodes between jobs: this script asks for no cores, so Slurm gives
          it 1 core and about 8 GB on standard. Ask for the cores it needs ...
  warning calls python without loading python-sci/python-ml or an environment
```

```text
bifrost status                    nodes powered up/down, queue, $/hour being spent now
bifrost partitions                partitions, cores, memory, GPUs, prices, what each is for
bifrost jobs [--state FAILED]     your jobs (queue + recent accounting)
bifrost job show 236              merged squeue + sacct record, efficiency, steps
bifrost job explain 236           deterministic diagnosis with evidence and a fix
bifrost job log 236 [--stderr]    redacted log window (--start N to page, --grep RE to search)
bifrost modules lammps            module search (versions, MPI prerequisite, GPU builds)
bifrost module hdf5/1.14.6        what a module sets (MPI-built ones under their MPI; --mpi mpich)
bifrost recipes pytorch           known-good recipes from the cluster catalog
bifrost check job.sbatch          static check of a batch script + worst-case cost (exit 2 on errors)
bifrost usage --since now-30days  node-hours, core-hours, efficiency, estimated cost
bifrost waste --since now-30days  avoidable spend: idle cores, idle nodes, repeat failures
bifrost results 265 [--download]  paged listing / chunked read / grep / download of a job folder
bifrost link 265 out.csv          signed download links for job outputs (staging bucket)
bifrost upload data.csv           stage an input file; then submit --input <upload id>
bifrost storage                   space used in your home and scratch folders
bifrost ls ~/project              a folder under your home or scratch (hidden files excluded)
bifrost cat ~/project/notes.txt   a text file (--offset to page, --grep RE to search)
bifrost env --module gcc python3  whether modules load and which tools you get
bifrost interactive -p gpul4 ...  the exact srun/salloc command for an interactive session
bifrost submit job.sh             plan, show worst-case cost, ask, submit           (tier A1)
bifrost cancel|hold|release 265   two-step, own jobs only                           (tier A1)
bifrost health                    down/drained nodes, slow boots, long-pending jobs (tier R2)
bifrost ticket 236 [--text f.txt] reply draft for a "my job failed" ticket          (tier R2; never sent)
bifrost serve                     the hosted MCP server (HTTP + Google sign-in)
bifrost config init|show|path     bifrost doctor     bifrost version
```

### Submitting and getting results (tier A1)

Add `A1` to `tiers` in the config. Then:

```text
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

### Backend: per-user IAP (`backend: iap`)

bifrost can reach the login node itself, as the Google user, with no gcloud call per
connection: it registers one OS Login key on your profile (8 h expiry, reused, stored 0600
under `~/.local/share/ursa-bifrost/iap-keys/`), opens an IAP TCP tunnel and logs in over
SSH with the login node's host keys pinned. Access is whatever Google already allows you
(IAP tunnel + OS Login). This is the same path the hosted server uses for each signed-in
user ([docs/CLOUD_PLAN.md](docs/CLOUD_PLAN.md)).

```yaml
backend: iap
iap:
  project: ucr-ursa-major-hpc-cluster
  zone: us-central1-a
  instance: ucrslurmcl-slurm-login-001
  host_keys: ["ssh-ed25519 AAAA...", "ecdsa-sha2-nistp256 AAAA..."]
```

## Tools

Tools come in tiers. A caller only sees, and can only call, the tiers granted to them
(`tiers` in the local config, `users.yaml` on the hosted server).

| Tier | Who | Tools |
|---|---|---|
| **R1** read | everyone | `cluster_status`, `partitions`, `jobs_list`, `job_show`, `job_explain`, `job_log_tail`, `job_results`, `modules_search`, `module_show`, `recipes`, `script_check`, `my_usage`, `waste_report`, `storage_usage`, `files_list`, `files_read`, `env_check`, `interactive_help`, `results_link`* |
| **A1** act | opt-in per person | `job_submit` (optional `inputs`) + `job_submit_confirm`, `job_cancel` / `job_hold` / `job_release` + `_confirm`, `upload_prepare`*, `uploads_list`* |
| **R2** staff | Research Computing | `jobs_list_all`, `job_show_any`, `job_explain_any`, `usage_report`, `waste_report_all`, `health`, `ticket_draft` |

\* when a staging bucket is configured. Also: resources `hpc://catalog` and
`hpc://policies`; prompts `diagnose_job`, `write_batch_script`, `monthly_usage_summary`,
`triage_ticket`.

Every result is an envelope with the data, the evidence, and the scheduler commands that
produced it, so an answer can be checked rather than trusted.

## Safety model

bifrost assumes the model on the other end will sometimes be wrong, and will
occasionally be manipulated by text it reads.

- **No shell, ever** ([SPEC](docs/SPEC.md) 18.5). Remote commands can only be built by
  constructors in `internal/backend/command.go` and `files.go`, each validating its
  arguments; shell templates are fixed text and caller values arrive only as positional
  parameters. `TestAllowListIsClosed` pins the allow-list, and
  `scripts/mutation_check.sh` disables each of 116 guards in turn and requires a test
  to fail.
- **Read-only by default; acting takes two steps.** State changes exist only in tier A1.
  A prepare call stores the exact action under a random single-use token (10-minute
  expiry, bound to the plan hash); the confirm call accepts nothing but the token. Caps
  are checked at prepare and the day cap again at confirm. Read tools carry
  `readOnlyHint: true`, act tools `false`.
- **Your files only.** Reads are limited to the caller's own home, scratch and job
  folders; hidden and credential-like files are refused and left out of listings;
  symlinks are resolved and checked again. Log paths come from the job's accounting
  record, never from the caller, and must sit under `log_roots` for the job's owner.
- **Uploads and downloads** go through short-lived signed Cloud Storage links in a private
  7-day staging bucket, under a folder derived from the signed-in identity; the job
  fetches its own inputs when it starts, so compute nodes hold no credentials.
- **Untrusted text is labelled.** Anything written by users or programs (log lines,
  scripts, submit lines) comes back only in `untrusted` blocks with a note telling the
  model it is data, not instructions.
- **Secrets are redacted** (tokens, keys, passwords, JWTs, private keys, credentials in
  URLs) from every output and from the audit log.
- **Everything is audited.** Every call, CLI or MCP, appends a JSON line (mode 600):
  caller, client, tool, redacted args, decision, commands, size, duration.
- **Low load.** Short-TTL cache, per-user rate limits, at most 8 concurrent SSH sessions
  per connection (the login node allows 10); no background polling.
- **Other users' jobs** are denied unless the caller has tier R2.
- **Shared partitions** ([SPEC](docs/SPEC.md) 19): which partitions share nodes is read
  from Slurm. On a shared one, a script must ask for its cores (`--cpus-per-task`,
  `--ntasks-per-node` or `--exclusive`) or `script_check` and `job_submit` refuse it, and
  costs are the share of the node the job holds.

The hosted server adds Google sign-in restricted to ucr.edu and to the people in
`users.yaml` (re-read on every request), OAuth 2.1 with PKCE and dynamic registration,
rotating refresh tokens (30 days), sealed token storage, and per-user IAP tunnels and
OS Login keys, so each person reaches the cluster as themselves. Programs (a dashboard,
a workstation app) can use a pre-registered client from `users.yaml` with a tier ceiling
and their own call budget, so a program never holds more than it needs
([SPEC](docs/SPEC.md) 21).
## Architecture

```text
 Claude / Codex / Gemini / Hermes / OpenCode / your scripts
        |  MCP: Streamable HTTP + OAuth 2.1 (hosted)   or   stdio (local)
        v
 +----------------------------- bifrost ------------------------------+
 | mcpserver    tools, resources, prompts                              |
 | core.Call    tier check, rate limit, cache, result envelope, audit  |
 | rules        deterministic job diagnosis (job_explain)              |
 | policy       redaction, untrusted wrapping, caps                    |
 | backend      allow-listed command constructors, nothing else        |
 +--------------------------------------------------------------------+
        |  one multiplexed SSH connection (local)
        |  per-user IAP tunnel + OS Login key (hosted)
        v
 Slurm login node  ->  squeue / sacct / sinfo / scontrol --json, module, sbatch
```

### Web dashboard (ursa-agent)

`agent/` is a separate Cloud Run service: sign in with Google through bifrost and get a
read-only view of the cluster as yourself: cluster pulse, your jobs (with `job_explain`
for failures), usage and estimated cost, waste, storage, partitions with an
interactive-session command builder, software search, staged files, and for staff
(tier R2) health, all jobs, usage and waste by user. Panels call bifrost tools directly
(no model). The assistant (Gemini through the UCR AI gateway) sits in a drawer and plans
jobs that only run after you press Approve. Design: [docs/SPEC.md](docs/SPEC.md)
section 20; deploy: [docs/DEPLOY.md](docs/DEPLOY.md).

## Self-hosting on your own cluster

Nothing in bifrost is specific to UCR except configuration: the login node, the catalog
(partitions, prices, modules, recipes), log roots and caps. Start with
`bifrost config init`, point `ssh` or `iap` at your login node, and write a catalog. The
hosted Cloud Run deployment (Secret Manager, sealed token store, users file) is
documented step by step in [docs/DEPLOY.md](docs/DEPLOY.md); `deploy/deploy.sh` does it
idempotently.

## Development

```bash
make check                  # gofmt, go vet, go test -race, build   (before every commit)
scripts/mutation_check.sh   # every safety guard must be caught by a test
scripts/mcp_smoke.py        # drives `bifrost mcp` over stdio like a real client
```

Tests run against anonymized recordings of real Ursa Major output in `testdata/`
(Slurm 25.11.4, data_parser v0.0.44). `scripts/make_fixtures.py` regenerates them from a
read-only probe and refuses to write if a real username survives; CI checks again.

Contributing: [CONTRIBUTING.md](CONTRIBUTING.md) · Security: [SECURITY.md](SECURITY.md) ·
Changes: [CHANGELOG.md](CHANGELOG.md) · Design, phases and open questions:
[docs/SPEC.md](docs/SPEC.md)

## About

Built by [UCR Research Computing](https://github.com/UCR-Research-Computing) for the
Ursa Major cluster on Google Cloud. Released under the [MIT License](LICENSE). The name: a bridge between two worlds, the assistant
on one side and the scheduler on the other, with a narrow, well-guarded crossing between.
