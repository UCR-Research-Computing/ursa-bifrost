# ursa-bifrost feedback from the Driftline session (2026-11-04)

Everything below was hit while driving real work through the hosted `ursa` MCP tools.

## Gaps where I had to leave the MCP server

1. **[Shipped v0.10.6] No way to wait for or watch a job.** There is no `job_wait` or watcher tool. To know when a job ended I had to poll `jobs_list` in a loop, or fall back to the laptop `bifrost` CLI in a shell script. Ask: a `job_wait(job_ids, timeout_s)` that blocks until the jobs leave PENDING/RUNNING (or times out) and returns final state, exit code and the log tail.
2. **Uploads need curl outside MCP.** [Open] `upload_prepare` returns a signed URL, and the actual PUT has to run as `curl` in a terminal. Fine for agents with a shell, impossible for chat-only clients. Ask: accept small files (a few MB) inline as base64 in `upload_prepare` / a new `upload_put`, or document that a shell is required.
3. **Downloads need curl outside MCP.** [Open] `results_link` returns signed URLs that must be fetched with curl. Same issue as uploads.
4. **The laptop CLI has no staging bucket.** [Open] `bifrost upload` fails with "file staging is not configured on this bifrost (no staging.bucket)". CLI and hosted server disagree on which features exist.
5. **The laptop CLI breaks on an expired gcloud login; the hosted MCP doesn't.** [Open] Users see one tool work and the other fail with no explanation. The CLI error should say "run `gcloud auth login`" plainly (it does, buried in the gcloud text).

## Rough edges inside the MCP server

6. **Array jobs show up as N unrelated job ids.** [Open] A 6-task array (1650) appears as 1650-1655, and `job_show 1650` reports only one task. The job folder's per-task logs are `slurm-1650_<n>.out` but are looked up under different ids. Ask: group array tasks under the parent id in `jobs_list`, and let `job_show`/`job_log_tail` take `1650_3`.
7. **[Shipped v0.10.6] `job_log_tail` fails before the job writes its log.** On a just-started job it returns "not a regular file" instead of "no log yet". Ask: return an empty window with a `pending` note.
8. **[Shipped v0.10.6] `script_check` rejects modules that don't exist but gives no hint about containers.** `module load cuda` failed with "closest: casa, cdo, PASA". The cluster's answer for CUDA is the `cuda-12.4-devel.sif` container; the check could say so.
9. **[Shipped v0.10.6] No hint that `apptainer` needs `module load apptainer` on compute nodes.** The job ran `apptainer` directly and failed with "command not found". `script_check` could flag a bare `apptainer` call without the module load.
10. **[Shipped v0.10.6] Exit 0 on a job whose real work failed** was my script's fault, but a `job_explain` pass that scans the log for Python tracebacks would have flagged it (job 1657 ended COMPLETED/0 with an AttributeError traceback in the log).
11. **Plans always carry dollar figures.** [Open] Users work in allocations; the plan could lead with allocation % and keep dollars behind a flag (Chuck's preference, 2026-11-04).
12. **`job_explain` on job 1659 named the wrong cause.** [Open] (pip/transformers torch-disable + 'python: command not found' inside apptainer); explain should surface the last error lines verbatim.

## Worked well

- `upload_prepare` + `inputs=[...]` staging into the job folder.
- `files_list` and `job_log_tail grep` for checking per-task outputs.
- Plans start immediately when a warm node exists (job 1659 started in under a second).
- jobs_list returned 'parsing Slurm JSON: unexpected end of JSON input' for job_ids=[1661] at 15:00 UTC (job_log_tail worked at the same time)
