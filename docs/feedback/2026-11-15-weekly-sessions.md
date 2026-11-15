# Ursa Major & ursa-bifrost Weekly Session Review (2026-11-06 to 2026-11-15)

Empirical audit of **58 Hermes sessions** touching Ursa Major and `ursa-bifrost` between **Nov 6 and Nov 15, 2026**:
- **1,772** `mcp__ursa__*` tool calls across 37 MCP-active sessions
- **138** confirmed Slurm submissions (`job_submit` + `job_submit_confirm`)
- **146** signed-URL GCS transfers (`upload_prepare` / `results_link`)
- **38** sessions that still touched `bifrost` CLI or `gcloud compute ssh`

---

## 1. Workloads Run on Ursa Major This Week

| Domain | Jobs / Partitions | Outcome |
|---|---|---|
| **CephRDS benchmarks** | Jobs 1789–1794 (`standard`, `computehigh`) | Validated parallel S3/RADOS staging throughput; uncovered `--mem-per-cpu` x `--cpus-per-task` gap in `script_check`. |
| **Beacon 3 & Back on the Broomstick Cycles renders** | Jobs 1810–1940 (`gpul4`, `gpu1v1`) | Multi-camera Blender 4.x Cycles renders + audio muxing; surfaced multi-file upload/download friction (dozens of sequential signed URLs). |
| **Ironwake naval 3D assets (Hunyuan3D + Blender)** | Jobs 1950–2120 (`gpul4`, `gpu1v1`, `computehigh`) | Ship hulls, rigging, PBR baking, LODs; surfaced Slurm job-array rollup gaps (`job_show` / `job_wait` only seeing task 0). |
| **Skyjoust & OmsteadLive 3D asset pipelines** | Jobs 2130–2310 (`gpul4`, `gpu1v1`, `standard`) | Batch mesh/texture generation and glTF/FBX export; surfaced `job_wait` 180 s client timeout wall and `files_link` gap for `/scratch` outputs. |
| **CUDA `gsplat` sm_70 V100 builds** | Jobs 2315–2355 (`gpu1v1`, `gpul4`) | Compiled `gsplat` kernels inside `/apps/containers/cuda-12.4-devel.sif`; surfaced misleading `module load cuda` hint on `nvcc: command not found`. |
| **TPUv6e Trillium spot enablement & benchmarks** | Jobs 2364–2391 (`tpu6spot`, `tpuspot`) | JAX + PyTorch/XLA benchmarks on v6e; surfaced `/apps/docs/TPU_PORTING.md` unreadability via `files_read` and `--gres=gpu` error clarity on `tpu*`. |
| **SW4 3.0 seismic simulations** | Jobs 2369–2406 (`computehigh`, `standard`) | Multi-node MPI wave propagation; surfaced fallback partition list (`-p a,b`) resource check gap and `job_wait` timeout behavior. |
| **RL arena training & evaluation** | Jobs 2358, 2409–2444 (`gpul4`, `gpu1v1`, `spot`) | Multi-seed RL sweeps via Slurm job arrays; surfaced `requeued` visibility when jobs return to `PENDING (Resources)` after preemption. |
| **Atrium / OAuth hardening (PR #79, v0.10.11)** | Control plane (`bifrost-server`) | PKCE/origin/grant-family hardening and R2 burn-rate gating. |

---

## 2. Issues Found & v0.10.12 Fixes

| # | Observed Problem (Sessions Nov 6–15) | Root Cause | Resolution in v0.10.12 |
|---|---|---|---|
| **1** | **12 hard `job_wait` MCP timeouts at 180.1 s** (`MCP tool 'job_wait' timed out after 180s`) | Default `TimeoutS` was 300 s, exceeding standard 180 s MCP client RPC deadlines; slow `JobsList` polls near the deadline also overran. | Lowered `DefaultWaitTimeoutS` to **120 s**, bounded inner `JobsList` polls with a deadline reserve so `{timed_out: true}` returns cleanly, and wired MCP `notifications/progress` on each poll when `progressToken` is present. |
| **2** | **Slurm job arrays (`--array=0-N`) misreported in `jobs_list`, `job_show`, `job_explain`, and `job_wait`** | In Slurm 25.11 `--json`, each array task has its own numeric `job_id` (`1651`, `1652`, ...) with parent ID in `array_job_id` / `array.job_id`. `AcctJob` omitted `array`, `JobsList` filtered only on `job_id` prefix, `JobShow("1650_2")` matched task 0, and `JobWait(["1650"])` exited when task 0 finished. | Declared `AcctJob.Array`; `JobsList`, `JobShow`, `JobExplain`, and `JobWait` now match all tasks via `ArrayParentID()` / `ArrayStepID()`, return an `ArraySummary` (`total`, `completed`, `failed`, `running`, `pending`, `failed_tasks`), wait for every task, and pull failed logs (stdout + separate stderr) from the first failed task. |
| **3** | **Multi-file input staging required 2N tool + `curl` round-trips** | `upload_prepare` accepted only one `{filename, bytes}` per call and always required an external `curl -T` step even for tiny configs/scripts; archive bundles had to be manually unpacked in every batch script. | Extended `upload_prepare` with batch `files=[...]` (up to 20 files), inline `content_text` / `content_base64` direct staging (up to 256 KB), GCS `Put` retry on 429/5xx, and `job_submit(unpack_archives=true)` (bound into the A1 plan hash). |
| **4** | **Retrieving 20–100 render frames or `/scratch` artifacts forced SSH/tar fallbacks** | `results_link` linked at most 20 individual job-folder files and could not bundle directories or link files written to `/scratch/<user>` or `/home/<user>` outside the job folder. | Added `results_link(archive=true, prefix=..., pattern=...)` (streams `tar -czf - ... \| curl` directly to GCS with zero cluster temp files, up to 200 files) and new R1 tool **`files_link`** (`paths=[...]`, `archive=true`) for regular files under `/home/<user>` and `/scratch/<user>`. |
| **5** | **`files_read` / `files_list` rejected `.`, `./sub`, `/apps/docs/*`, and `/apps/containers`** | `resolveUserPath` rejected `.` segments (`path.Clean(p) != p`) and allowed only `/home/<user>` and `/scratch/<user>`, blocking `/apps/docs/TPU_PORTING.md` and container discovery. | Added `cleanDotSegments` (normalizes `.` while preserving `..` traversal rejection), opened `/apps/docs` to read-only `files_list`/`files_read` and `/apps/containers` to `files_list` (with symlink confinement inside public roots and an `apptainer exec` hint if binary `.sif` read is attempted). |
| **6** | **`jobs_list` / `my_usage` rejected common `since` shorthands (`now-1hour`, `24h`, `7d`, `2w`)** | `reSince` required exact plural `now-N(days\|hours\|minutes)`. | Added `backend.NormalizeSince` accepting `now-1hour`, `2h`, `30m`, `7d`, `2w`, etc., before `sacct` validation. |
| **7** | **`script_check` missed `--mem-per-cpu` x CPUs overflow, fallback-list (`-p a,b`) smaller-member limits, and `partition` override** | `ScriptCheck` checked `--mem` only, validated only the dearest member of `-p a,b`, and had no `partition` parameter on MCP. | Added `--mem-per-cpu * cpus > MemGBPerNode` check, per-member core/memory/GPU/access checks across comma-separated partition lists, `script_check(partition=...)`, and TPU-specific `--gres=gpu` error guidance. |
| **8** | **Laptop CLI / SSH backend broke under leaked `PYTHONPATH` and hid `gcloud` token expiry** | Inherited Python 3.14 `PYTHONPATH` crashed `gcloud` (`module 'lib' has no attribute 'GEN_EMAIL'`), and `ssh.go` printed only the last stderr line (`gcloud config set account`). | Stripped `PYTHONPATH` from `gcloud` / `ssh` subprocess environments in `backend/ssh.go` and `staging/staging.go`, and added `gcloudErrorLine` to surface `Reauthentication failed` directly. |
| **9** | **Diagnosis polish (`job_explain` / `cluster_status` / `partitionTrouble`)** | `CUDA out of memory` also triggered `oom-log` (suggesting CPU `highmem`), `PENDING` jobs with `RestartCnt > 0` and `Reason=Resources` hid prior requeues, `script-error` omitted log lines, `gpul4` stockouts suggested CPU `standard`, and `cluster_status` lacked a `partition` filter and `SuspendTime` context. | Suppressed `oom-log` on CUDA VRAM OOMs, tailored CUDA/nvcc hints to `gpul4` + `gpu1v1` + `cuda-12.4-devel.sif`, surfaced `requeued` on any `RestartCnt > 0` pending job, quoted last 3 log lines in `script-error`, made `partitionTrouble` GPU/TPU-aware, and added `cluster_status(partition=...)` + `idle_suspend_min`. |
