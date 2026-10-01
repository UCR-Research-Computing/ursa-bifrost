# ursa-bifrost

A read-only bridge between AI assistants and the Ursa Major Slurm cluster (UCR Research
Computing, Google Cloud). It is an MCP server and a CLI (`bifrost`) in one Go binary.

An assistant asks typed questions ("why did job 236 fail?", "what is billing right now?",
"which modules give me LAMMPS on GPU?", "check this batch script") and gets JSON back,
with the evidence and the exact scheduler commands that produced it. The server holds no
language model: it runs allow-listed scheduler queries and fixed diagnosis rules, and the
calling assistant does the reasoning.

Status: v0.1 (phase P1 of `docs/SPEC.md`): personal, read-only, SSH backend.

## Install

```bash
scripts/install.sh          # builds ~/.local/bin/bifrost (Go 1.25+; toolchain auto-fetched)
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
```

Every command takes `--json`. `bifrost check` exits 2 when the script has errors.

## MCP

Add to a client as a stdio server:

```json
{"mcpServers": {"ursa": {"command": "bifrost", "args": ["mcp"]}}}
```

Hermes (`~/.hermes/config.yaml`):

```yaml
mcp_servers:
  ursa:
    command: bifrost
    args: [mcp]
```

Claude Code: `claude mcp add ursa -- bifrost mcp`

Tools (R1): `cluster_status`, `partitions`, `jobs_list`, `job_show`, `job_explain`,
`job_log_tail`, `modules_search`, `module_show`, `recipes`, `script_check`, `my_usage`.
Staff tools (R2, only registered when `tiers` includes R2): `jobs_list_all`,
`job_show_any`, `job_explain_any`, `usage_report`.
Resources: `hpc://catalog`, `hpc://policies`. Prompts: `diagnose_job`,
`write_batch_script`, `monthly_usage_summary`.

## Safety model

- No shell passthrough. Commands can only be built by constructors in
  `internal/backend/command.go`, each validating its arguments; every argument is quoted.
  A test pins the allow-list (`squeue`, `sacct`, `sinfo`, `scontrol show`, `cat` of the
  catalog, `tail` of a job log, `module show`, `id -un`).
- Read-only. Nothing submits, cancels or changes state. Tools carry `readOnlyHint`.
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
