# ursa-bifrost: scaling plan

| | |
|---|---|
| Status | Draft 1, 2026-10-02. Phase 1 built (v0.9.1). |
| Target (Chuck, 2026-10-02) | 20 people at once with heavy harness use plus programs and scripts: plan for **1,000+ calls a minute** to the hosted server, bursts above that. Budget raised as needed. |
| Sibling | Nexus `2026-10-02_MCP_Scaling_Plan.md` (same phases) |

## 1. Where the time goes

bifrost is a thin Go server; almost all of a call's time is the command it runs
on the login node over SSH (IAP tunnel, OS Login), as the person. Measured
2026-10-02: `cluster_status` ~1-3 s, accounting reads ~7 s alone and 10-20 s
several at once, `storage_usage` 100 s cold. The Cloud Run instance (1 vCPU,
512 MiB, min 0, max 1, concurrency 40) is idle while it waits.

So the limits, in order, are:

1. **The login node and Slurm.** One shared login node (`MaxSessions 10` per
   connection; bifrost uses at most 8 per person), slurmctld for `squeue`/`scontrol`,
   slurmdbd for `sacct`. Fifty people asking for usage reports at once is a
   scheduler problem, not a Cloud Run one.
2. **Repeated work.** Before v0.9.1 each person's Service cached on its own, so node
   state, the queue, partitions and the catalog (the same for everyone) were fetched
   once per person, and two identical calls at the same moment both ran.
3. **Cold starts.** min 0: after idle the first caller pays container start, the IAP
   tunnel and the SSH handshake (and the per-person cache is empty).
4. **One instance only.** In-progress sign-in state (pending Google logins, auth
   codes) and the per-person SSH connections live in memory; the A1 spend ledger is a
   file lock on the bucket mount. A second instance would break sign-ins that land on
   the other copy and could race OS Login key imports.

More vCPUs do not help: Go already uses every core, and the work is remote.

## 2. Phases

### Phase 1: stop repeating work (v0.9.1, built)

- `core.SharedCache`, one per server: output marked `Public()` (`scontrol show nodes`,
  `squeue --json` for everyone, `sinfo`, the partition sharing list, the site
  catalog) is cached **once for everyone** at its usual TTL (queue 20 s, nodes 30 s,
  catalog 1 h).
- Single-flight: identical commands in flight run once. Public output is keyed for
  everyone; any other output only for that person and program, so two people never
  share an answer (each runs it as themselves).
- Never shared or coalesced: per-person output (their jobs, files, accounting,
  modules), and every write (A1). Errors are not stored.
- Tests: two people share node state but not their jobs; 20 concurrent identical
  calls run once; private runs at the same moment stay separate; writes always run;
  stale entries are refetched. 5 mutation guards.

### Phase 2: protect the cluster, then warm

- **min 1** (always warm; ~$10-20/month at 512 MiB): no cold start, SSH connections
  and caches survive between calls. Config only.
- **Per-person in-flight cap** in front of the 8 SSH sessions, refusing with a clear
  "busy, retry" instead of queueing behind a runaway script.
- **Global accounting budget**: cap concurrent `sacct` runs across all people (e.g.
  4), since slurmdbd is shared; queue the rest briefly.
- Longer TTLs where the data allows (accounting summaries for past days do not
  change: cache by day).

### Phase 3: more than one instance (only if Phase 2 numbers call for it)

- Pending sign-ins and auth codes move to the sealed store (any instance can finish a
  sign-in).
- Spend ledger lock that works across instances (or act tools pinned to one).
- Single owner per person's OS Login key (or per-instance keys with expiry).
- Then max instances > 1 with session affinity off.

### Phase 4: cluster side (Chuck's call)

If load still grows: precomputed usage summaries refreshed on a schedule, a
dedicated API/login node for bifrost, or slurmrestd instead of SSH. These change
the cluster, not bifrost.

## 3. Change log

| Date | Change |
|---|---|
| 2026-10-02 | Draft 1: limits, phases; Phase 1 (shared cache + single-flight) in v0.9.1 |
