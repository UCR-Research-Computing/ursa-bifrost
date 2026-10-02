# ursa-bifrost on Cloud Run: a real MCP server and a Gemini agent

| | |
|---|---|
| Status | Plan for approval, 2026-10-01. Nothing deployed, no API enabled. |
| Owner | Chuck Forsyth (UCR Research Computing) |
| Repo | github.com/UCR-Research-Computing/ursa-bifrost (this plan: `docs/CLOUD_PLAN.md`) |
| Builds on | bifrost v0.3.2 (CLI + stdio MCP, 28 tools, tiers R1/R2/A1), `docs/SPEC.md` |
| Decisions so far | Project `ucr-ursa-major-hpc-cluster`. Launch users: Chuck only. "Real agent" = Gemini-powered, built to register in Gemini Enterprise later. |

## 1. Goal

Today bifrost runs on Chuck's laptop as a child process of Hermes and reaches the cluster with
his `gcloud` login. The goal:

1. **A remote MCP server** on Cloud Run (Streamable HTTP) that any MCP client (Hermes, Claude
   Code, Gemini CLI, the agent below) can connect to with a URL and a UCR sign-in.
2. **A Gemini agent** on Cloud Run that uses that MCP server as its tool set, has its own chat
   page, and speaks A2A so a Gemini Enterprise admin can register it later without changes.
3. **Every cluster action runs as the signed-in person**, never as a shared service account.

## 2. How the server acts as the user: two options checked

### 2.1 Option A: IAP + OS Login per user (prototype works)

The server holds the caller's Google OAuth access token and does what
`gcloud compute ssh --tunnel-through-iap` does, in Go:

1. OS Login API `users/{email}:importSshPublicKey`: a fresh ed25519 key, 5-minute expiry;
   the response gives the user's POSIX username.
2. IAP TCP forwarding WebSocket (`wss://tunnel.cloudproxy.app/v4/connect`, subprotocol
   `relay.tunnel.cloudproxy.app`) to `login-001:22` with the caller's bearer token.
3. SSH as that POSIX user; run the same allow-listed command constructors bifrost already uses.
4. Delete the key (named by sha256 of the exact imported line).

Prototype: `prototypes/iap-oslogin/main.go`. Live result 2026-10-01: 2.5-3.5 s cold
(OS Login 1.1 s, IAP 0.4-1.2 s, SSH 0.6 s); `id -un` returned Chuck's own POSIX user; the key
was removed (profile back to 4 keys). Finding while testing: deleting by the key-blob hash
returns HTTP 200 and deletes nothing; the ID is the hash of the full imported line.

Authorization is entirely Google's: IAP `tunnelResourceAccessor` + OS Login
(`roles/compute.osLogin`) + the `slurm-login` group, exactly who can SSH today. Nothing on the
cluster changes.

Scopes the user grants: `openid email` + `https://www.googleapis.com/auth/cloud-platform`
(IAP tunnel and OS Login have no narrower scope). That is a broad grant; see 6.2.

### 2.2 Option B: slurmrestd (checked against the official 25.11 docs)

We run Slurm 25.11.4. Sources: the Slurm 25.11 archive docs
(`slurm.schedmd.com/archive/slurm-25.11.0/`: rest.html, rest_quickstart.html, jwt.html,
slurmrestd.html, rest_clients.html, rest_api.html); the live site now documents 26.05.
The 25.11 line is at 25.11.8 (we are on .4).

What the 25.11 REST API can do (data_parser v0.0.44, 67 method+path pairs): submit
(`POST /slurm/v0.0.44/job/submit`), allocate, show/update/cancel a job
(`GET|POST|DELETE /slurm/v0.0.44/job/{id}`), bulk cancel (`DELETE /jobs/`), jobs, nodes,
partitions, reservations, shares, licenses, diag; and slurmdbd: accounting jobs, users,
accounts, associations, QOS, TRES, wckeys, clusters. It covers every bifrost read and act tool
except three that are not Slurm calls at all: log tails and job results (files), and the
module/catalog search (`/apps/docs/catalog.json`). It has no dry run: the v0.0.44 job
description has no `test_only` field, so `sbatch --test-only` (our plan step) has no REST
equivalent.

What the docs say about acting as a user (the part that decides this):

- "The power to create JWTs is the power of root on a cluster." (jwt.html)
- An authenticating proxy needs "a JWT token assigned to the SlurmUser that can then be used to
  proxy for any user on the cluster ... Slurm will explicitly trust the HTTP headers provided"
  (`X-SLURM-USER-NAME`). (rest.html)
- JWKS/RS256 lets an external IdP sign tokens; "Those trusted keys can then create new tokens
  ... for any user". `userclaimfield=` picks the claim used as the Slurm user name. (jwt.html)
- slurmrestd "is not designed to be directly internet facing ... no protection against man in
  the middle or replay attacks"; put a TLS 1.3 authenticating proxy in front and keep it on a
  trusted network. (rest.html) slurmrestd can do TLS itself via `tls/s2n`; our build has only
  `tls/none`.
- It cannot run as root or SlurmUser by default; `become_user` exists only for UNIX-socket
  inet mode and "is incompatible with rest_auth/jwt". (slurmrestd.html)

What the cluster has now (checked read-only today): `AuthAltTypes=auth/jwt` with no
`AuthAltParameters` (so no `jwt_key`/`jwks` configured), `slurmrestd.service` installed and
disabled, plugins `rest_auth/jwt`, `rest_auth/local`, `openapi/slurmctld|slurmdbd|util`, data
parsers v0.0.41-44, TLS `tls/none`. `scontrol token` works for a normal user.

Three ways bifrost could use it, all requiring us to enable and run slurmrestd on a VM in
`ucr-hpc-net` and reach it from Cloud Run with Direct VPC egress:

| | How the user is asserted | Who holds root-equivalent power |
|---|---|---|
| B1 SlurmUser proxy token | bifrost sends `X-SLURM-USER-NAME: <posix user>` with a SlurmUser JWT | bifrost (a leaked secret = any user's jobs) |
| B2 HS256 key, mint per user | bifrost signs a short JWT per user with the cluster `jwt_key` | bifrost (holds the key) |
| B3 JWKS with a key bifrost owns | bifrost signs RS256 tokens per user with a KMS key Slurm trusts | bifrost (KMS signing permission) |

All three are the same trust model the docs describe: the server becomes the authority on who
the user is, with root-equivalent power over every account. There is no mode where Slurm
verifies the end user's own Google identity: Google ID tokens put the email in `email`, Slurm
needs the POSIX name (`<netid>_ucr_edu`), and anyone who can obtain a Google ID token for that
audience could present one, so trusting Google's JWKS directly is not safe either.

### 2.3 Recommendation

**Use Option A as the identity path; add slurmrestd later as an optional speed-up for reads,
not as the way users are asserted.**

| | A: IAP + OS Login | B: slurmrestd |
|---|---|---|
| Acts as the user | Yes, with the user's own Google credentials; Google decides | bifrost asserts the user with a root-equivalent secret |
| Blast radius if bifrost is compromised | Only users with live sessions, only what they can already do | Every account on the cluster |
| Cluster changes | None | Enable slurmrestd, JWT key or JWKS, a VM or the login node, firewall, TLS |
| Latency | 2.5-3.5 s cold, ~0.3 s on a reused connection | ~50-100 ms per call |
| Logs, results, catalog, dry run | Yes (same as today) | No (still need A for these) |
| Who can use it | Anyone who can SSH today | Anyone with a POSIX account, decided by bifrost |

slurmrestd would make reads faster and give structured submit, but it would not make the
identity story better; it makes it worse, because it moves "who is this user" from Google into
our code. Since we run the cluster, we can turn it on any time if speed matters (section 7).

### 2.4 C1 findings (live, 2026-10-01)

Built as `backend.IAP` (`internal/backend/iap.go`), selectable on the laptop with
`backend: iap`. What live testing changed from the prototype:

| Finding | Change |
|---|---|
| The login node caches a user's OS Login key list. A key imported seconds after a lookup is rejected for ~5-30 s (plain OpenSSH shows the same). One fresh key per connection made cold calls 7-30 s, sometimes failing. | One key per user, 8 h OS Login expiry, reused across connections and processes (laptop: `~/.local/share/ursa-bifrost/iap-keys/`, 0600; server: encrypted store). Replaced 10 min before expiry; the old one is deleted. `Revoke` removes it (sign-out, user removed). OS Login expires it even if bifrost never runs again. |
| Key id is sha256 of the full imported line (comment included); the blob hash returns 200 and deletes nothing. | `keyID(line)`; import is refused if the key is not found under that id. |
| Parallel calls each imported a key. | Single-flight connection setup. |
| `x/crypto/ssh` writes stdout and stderr from two goroutines; a shared buffer raced. | Locked writer for merged output. |
| A connection lost mid-command must not re-run a state-changing command. | Write commands are never retried; the error says to check state first. |
| Host keys are not in guest attributes on this image. | Pinned from `/etc/ssh/ssh_host_*_key.pub` (config `iap.host_keys`). |

Measured with key reuse: first call ~2.5 s (import + tunnel + handshake), later calls
1.8-4.4 s each as separate CLI processes (tunnel + handshake ~1 s, then the command; sacct
dominates `jobs`). Within one process (the server) calls reuse the open SSH connection:
~0.2 s plus command time.

Option for later (needs a cluster change, not done): `enable-oslogin-certificates=TRUE` on the
login node lets bifrost use `signSshPublicKey` short-lived certificates instead of profile keys,
which removes the propagation delay and the stored key entirely.

### 2.5 C2/C3 status (v0.5.1)

`bifrost serve` (internal/server) is built, tested and **deployed** on Cloud Run (C3 done 2026-10-01; see docs/DEPLOY.md "Live deployment"). Hermes uses it as its `ursa` MCP server.

- MCP over Streamable HTTP at `/mcp` (stateless, JSON responses). A 401 includes
  `WWW-Authenticate: Bearer resource_metadata=...` for discovery.
- bifrost is the OAuth 2.1 authorization server: RFC 9728 protected-resource
  metadata, RFC 8414 AS metadata, RFC 7591 dynamic client registration (public
  clients, https or loopback redirects only), authorization code with PKCE
  S256 (required), single-use 5-minute codes, rotating refresh tokens bound to
  the client, RFC 7009 revocation, rate limits on register and token.
- Sign-in hands off to Google (openid, email, cloud-platform, `hd=ucr.edu`,
  PKCE). The ID token is verified against Google's keys (RS256, issuer,
  audience, expiry, verified email, hosted domain). The person must be on
  `users.yaml`, which is checked on every request, so removing someone takes
  effect immediately.
- No token passthrough. bifrost tokens never go to Google and Google tokens
  never go to clients. The Google refresh token and each person's SSH key are
  sealed with AES-256-GCM, with the record kind and owner as associated data so
  a record cannot be swapped into another slot, and stored as 0600 files.
- Per person: their own tiers (the MCP server only lists the tools they may
  use), caps, ledger file and IAP backend with their own Google token. The
  audit log names the person. `job_results` download is off on the server.
- Sign-out (`POST /signout`) deletes the Google token and the SSH key from the
  store and from OS Login. Revoking access in the Google account ends the
  session on the next refresh.
- Tests: fake Google (RS256 ID tokens, refresh, revocation), the full OAuth
  flow with a real MCP client, two users kept apart, removal and tier changes,
  code, PKCE, redirect and ID-token attacks, sealed storage. The mutation
  check covers 13 server guards.
- Deploy: Dockerfile (distroless, static), deploy/deploy.sh (plan/apply,
  asks before each billable or IAM change), docs/DEPLOY.md. Manual step: the
  Google OAuth client, because the IAP OAuth Admin API was shut down in March 2026.

## 3. Architecture

```
  Hermes / Claude Code / Gemini CLI          Browser            Gemini Enterprise (later)
            |  MCP (Streamable HTTP)           |  chat               |  A2A + user OAuth
            v  Authorization: Bearer <bifrost  v                     v
  +-----------------------------------+   +------------------------------------------+
  | bifrost-mcp  (Cloud Run, Go)      |<--| ursa-agent  (Cloud Run, Python ADK)      |
  |  OAuth 2.1 authorization server:  |MCP|  Gemini via ucr-gateway (LiteLLM)        |
  |   Google sign-in, ucr.edu only,   |   |  tools = bifrost-mcp over MCP            |
  |   allow-list, PKCE, DCR           |   |  chat page + A2A endpoint + agent card   |
  |  28 tools, same policy/caps/audit |   +------------------------------------------+
  |  Backend: IAP + OS Login per user |
  +-----------------------------------+
     |                |              |
     | Firestore      | Secret Mgr / | Cloud Logging
     | sessions,      | KMS: refresh | audit (one entry per call)
     | tokens, ledger | token crypto |
     v
  IAP (tunnel.cloudproxy.app) --> login-001:22 as the user (OS Login)
```

### 3.1 bifrost-mcp (Go, Cloud Run)

- Same binary, new mode: `bifrost serve --http :8080`. The go-sdk v1.8.0 already provides the
  Streamable HTTP transport and auth middleware (checked during P1).
- **Auth (MCP 2025-06 authorization spec):** bifrost is an OAuth 2.1 authorization server for
  MCP clients. It publishes protected-resource and authorization-server metadata, supports PKCE
  and dynamic client registration (Hermes has an OAuth client for MCP, including device flow),
  and federates the actual login to Google:
  - Google OAuth client (Web), `hd=ucr.edu`, scopes `openid email cloud-platform`,
    `access_type=offline`.
  - bifrost checks the ID token (`email_verified`, `hd == ucr.edu`) and an allow-list
    (launch: Chuck only), then issues its own short-lived access token (15 min) and refresh
    token bound to that client. **It never passes the client's token to Google and never
    accepts a Google token from a client** (the MCP spec forbids token passthrough).
  - The user's Google refresh token is stored encrypted (Cloud KMS envelope) in Firestore,
    used to mint 1-hour Google access tokens for IAP/OS Login, and deleted on sign-out or after
    30 days idle.
- **Backend:** new `backend.IAP` alongside `SSH` and `Fixture`, built from the prototype plus:
  pinned host key (from `gcloud compute instances get-guest-attributes`), the corrected key
  cleanup with a verify step, one SSH connection per user kept for 5 minutes (calls drop to
  ~0.3 s), and the existing allow-list command constructors unchanged.
- **State:** the A1 ledger and confirm tokens move from `a1.json` + flock to Firestore
  transactions, keyed per user (caps per user, not per laptop). Confirm tokens stay single use,
  bound to the plan hash and to the user who prepared them.
- **Results:** `job_results` downloads become signed, time-limited links (or inline small files)
  instead of writing to `~/ursa-results`.
- **Audit:** every call to Cloud Logging with user, tool, decision, commands, duration; secrets
  redacted as today.

### 3.2 ursa-agent (Python ADK, Cloud Run)

- Google ADK agent; model via the existing AI gateway (LiteLLM on Cloud Run, PSSA billing),
  default `gemini-3.8-flash`, no API keys in code (gateway key in Secret Manager).
- Tools: `McpToolset` pointing at bifrost-mcp. The agent signs in to bifrost as the user
  (OAuth), so the agent never holds cluster power of its own.
- Instructions: answer cluster questions, diagnose jobs, draft scripts, run script_check; for
  any submit or cancel, show the plan and wait for the user's explicit yes before calling
  `*_confirm`.
- Surfaces: a small chat page (`/`), and an A2A endpoint with an agent card
  (`/.well-known/agent-card.json`), so registering in Gemini Enterprise is configuration only.
- Gemini Enterprise later: it registers A2A agents hosted on Cloud Run; it sends a
  service-agent OIDC token in `X-Serverless-Authorization` (Cloud Run IAM) and the end user's
  OAuth token in `Authorization`. The agent then exchanges that for a bifrost session. Needs a
  Gemini Enterprise admin and an app; not in this phase.

## 4. GCP resources (project `ucr-ursa-major-hpc-cluster`, `us-central1`)

| Resource | Purpose | Cost note |
|---|---|---|
| Enable `run`, `cloudbuild`, `firestore`, `secretmanager`, `cloudkms`, `iap` (API only) | | free to enable |
| Artifact Registry repo `bifrost` | images | cents |
| Cloud Run `bifrost-mcp`, `ursa-agent` | min instances 0, max 3 | near zero when idle |
| Firestore (Native) | sessions, tokens, ledger | free tier likely |
| KMS key ring `bifrost`, key `token-wrap` | encrypt refresh tokens | ~$0.06/month |
| Secret Manager | Google OAuth client secret, gateway key | cents |
| Service accounts `bifrost-mcp-sa`, `ursa-agent-sa` | runtime | no cluster access; only Firestore, KMS decrypt, logging |
| OAuth client (Web) on the existing consent screen, internal | Google sign-in | free |

No VPC connector or Direct VPC egress needed for option A: IAP is a public Google endpoint.
No new firewall rules; the existing `ucr-hpc-net-fw-allow-iap-ingress` (35.235.240.0/20 to
tcp:22) is what the prototype used.

## 5. Phases

| Phase | What | Done when |
|---|---|---|
| C1 IAP backend | `backend.IAP` in bifrost (prototype hardened: host key pin, verified key cleanup, connection reuse, timeouts), CLI flag `--backend iap`, tests with a fake IAP relay and fake OS Login | Laptop bifrost works with `--backend iap` and no gcloud binary; mutation check covers key cleanup |
| C2 HTTP + OAuth | `bifrost serve`: Streamable HTTP, OAuth AS federated to Google, allow-list, Firestore state, Cloud Logging audit; local run against Firestore emulator | Hermes connects to a local `bifrost serve` with OAuth and runs every tool as Chuck |
| C3 Deploy MCP | Enable APIs, AR, KMS, Firestore, SAs, OAuth client, Cloud Run `bifrost-mcp` | Hermes on the laptop uses the Cloud Run URL; same live test campaign as v0.3.1 passes |
| C4 Agent | `ursa-agent` (ADK), chat page, A2A card, deploy | Chat page answers, diagnoses a failed job, submits only after a yes |
| C5 Gemini Enterprise | Register as an A2A agent (needs a GE admin and app) | Agent visible in GE for Chuck |
| C6 Optional slurmrestd reads | Only if speed matters: slurmrestd on the controller network, read-only endpoints, B3 tokens limited to `GET` by a proxy | p50 read latency < 200 ms |

Each phase ships through a PR with CI, a version bump and a SPEC update, like P1-P3.

## 6. Risks and open questions

1. **IAP protocol is not a published API.** gcloud's Python client is the reference; the relay
   protocol (connect/data/ack frames) has been stable for years and is also implemented by
   Google's IAP Desktop. Mitigation: keep the SSH-via-gcloud backend as a fallback; a
   contract test that runs the prototype nightly is cheap.
2. **Broad scope.** `cloud-platform` lets bifrost act on anything the user can touch in GCP,
   not only the cluster. Mitigations: tokens stay server-side, encrypted, used only by the IAP
   and OS Login calls in one package (enforced by a test that greps for other Google API
   hosts), short-lived access tokens, sign-out deletes the refresh token. Open: do we want a
   separate Google OAuth client per bifrost deployment so consent screens say exactly this?
3. **UCR Google Workspace policy** may restrict third-party OAuth apps requesting
   `cloud-platform`. An internal OAuth client in a UCR project usually passes; confirm with
   whoever administers UCR's Google Cloud org.
4. **Every POSIX account is `<netid>_ucr_edu`.** OS Login derives it; nothing to map.
5. **Cold start.** Cloud Run min 0 means a few seconds on the first call after idle; min 1 costs
   about $10-15/month if that matters.
6. **Who besides Chuck, and when** (launch is Chuck only).

## 7. If we later want slurmrestd anyway

Concrete steps, from the 25.11 docs, for the record:
- Generate an RS256 key in Cloud KMS; write its public JWK to
  `/var/spool/slurm/jwks.json` (0400, slurm-owned) on the controller and slurmdbd host; set
  `AuthAltParameters=jwks=/var/spool/slurm/jwks.json,userclaimfield=sun` (`sun` is the claim
  Slurm reads by default), keep `jwt_key=` too if `scontrol token` must keep working.
- Run `slurmrestd` as a dedicated unprivileged user, `SLURM_JWT=daemon`, listening on the
  controller's internal IP, `-s slurmctld,slurmdbd -d v0.0.44`; firewall tcp:6820 from the
  Cloud Run egress subnet only.
- Cloud Run: Direct VPC egress into `ucr-hpc-subnet`; bifrost signs a 60-second RS256 token per
  call for the signed-in user only.
- Make the change in the blueprint (`ucr-slurm-production`), not by hand.

## 8. Not doing

- No shared service account that SSHes to the cluster.
- No token passthrough from MCP clients to Google or Slurm.
- No admin tools; R2 stays staff-only, A1 caps as today.
- No public unauthenticated endpoints except OAuth metadata and the agent card.
