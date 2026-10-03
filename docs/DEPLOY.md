# Deploying the hosted ursa-bifrost (C2/C3)

The hosted server is `bifrost serve`: the same MCP tools over Streamable HTTP at
`<url>/mcp`, with OAuth sign-in through Google. Every cluster call runs as the
signed-in person through IAP + OS Login, with their own Google token. The Cloud
Run service account has no cluster access.

## How users are separated and controlled

| Layer | Who controls it | What it does |
|---|---|---|
| Google sign-in | UCR Google Workspace | Only verified `ucr.edu` accounts can sign in |
| `users.yaml` | Research Computing | Who may use bifrost, their tiers (R1/R2/A1) and caps; checked on every request |
| IAM + OS Login | GCP project IAM | Whether the person can reach the login node at all, same as `gcloud compute ssh` |
| Slurm | The cluster | Jobs run as the person's own POSIX account, with their accounts and QOS |

To remove someone, delete them from `users.yaml` (or set `disabled: true`) and
upload it again. Their next request is refused and their refresh tokens stop
working. Each person has their own ledger and daily cap. Results are not
downloaded on the server (`job_results` lists and reads files only).

## One-time manual step: the Google OAuth client

The IAP OAuth Admin API has been shut down (March 2026), so this can only be
done in the console:

1. Console, Google Auth Platform, project `ucr-ursa-major-hpc-cluster`. Branding:
   app name "ursa-bifrost", Audience **Internal** (UCR accounts only).
2. Data access: add the scopes `openid`, `email` and
   `https://www.googleapis.com/auth/cloud-platform`. bifrost needs
   cloud-platform for the IAP tunnel and OS Login key import, and it has no
   narrower scope (CLOUD_PLAN.md section 6).
3. Clients: Create client, type **Web application**, and tick "This client will be used
   by an AI-powered agent". Authorized redirect URIs:
   `https://bifrost-mcp-<project number>.us-central1.run.app/oauth/google/callback`
   (the Cloud Run URL is deterministic, so it is known before the first deploy) and
   `http://localhost:8080/oauth/google/callback` for a local sign-in test. Download the JSON.
4. Put the client ID in the config (`server.google_client_id`) and point `CLIENT_JSON`
   in deploy/env at the downloaded file; deploy.sh loads the secret into Secret Manager
   without printing it.

## Deploy

```
cp deploy/config.example.yaml ~/bifrost-deploy/config.yaml    # fill in host_keys, client id, base_url
cp deploy/users.example.yaml  ~/bifrost-deploy/users.yaml     # the people allowed
cat > deploy/env <<EOF                                        # git-ignored
PROJECT=ucr-ursa-major-hpc-cluster
REGION=us-central1
CONFIG=$HOME/bifrost-deploy/config.yaml
USERS=$HOME/bifrost-deploy/users.yaml
CLIENT_JSON=$HOME/.config/secrets/google/bifrost-oauth-client.json
EOF
deploy/deploy.sh plan      # read-only: shows what is missing
deploy/deploy.sh apply     # asks before each billable or IAM change
```

One instance is kept warm (min-instances 1, since v0.9.1 scaling phase 2): no cold
start, and the shared cache and sign-in state survive between calls. Idle cost is
about $10 a month (1 vCPU, 512 MiB, CPU billed only during requests) plus a few MB
in Cloud Storage and Secret Manager versions. `MIN_INSTANCES=0 deploy/deploy.sh
apply` returns to zero idle cost with cold starts.

## Connect a client

Hermes: `hermes mcp add ursa-cloud --url https://<service url>/mcp`. Hermes
discovers the OAuth server, registers itself, opens the browser for Google
sign-in, and stores bifrost's tokens (never the Google ones).

Sign out (deletes the stored Google token and SSH key):
`curl -X POST -H "Authorization: Bearer <token>" <url>/signout`, or remove
ursa-bifrost from https://myaccount.google.com/permissions. bifrost notices on
the next token refresh and ends the session.

## Live deployment (UCR, 2026-10-01)

- Service: `bifrost-mcp`, us-central1, https://bifrost-mcp-125853442225.us-central1.run.app
  (min 0 / max 1 instances, 1 vCPU, 512 MiB, gen2).
- Service account `bifrost-mcp@ucr-ursa-major-hpc-cluster.iam.gserviceaccount.com`:
  `secretAccessor` on the four bifrost secrets and `storage.objectUser` on
  `gs://ucr-ursa-major-hpc-cluster-bifrost-data`. No compute, IAP or OS Login roles.
- Verified live: Google sign-in (Internal app with the AI-agent flag), MCP over HTTP,
  calls ran on the login node as the person who signed in (first call 3.5 s, then
  0.1 s), audit in Cloud Logging names the person, the bucket holds only sealed records.
- Hermes: `hermes mcp add ursa --url <service url>/mcp --auth oauth`, then
  `trust: untrusted` so destructive tools ask for approval.
- Health check: `GET /health`. Google's front end answers `/healthz` itself on run.app.
- Staging (v0.7.0): `gs://ucr-ursa-major-hpc-cluster-bifrost-staging`, private (uniform access,
  public access prevention), 7-day delete rule, no soft delete, CORS `PUT` from the ursa-agent
  origin only. The service account has `storage.objectAdmin` on that bucket and
  `iam.serviceAccountTokenCreator` on itself (signs links through `signBlob`; no key file).
  Verified live: a signed upload works, a body over the signed size is refused (400), a changed
  size header breaks the signature (403), unsigned and bad-signature reads are refused (403).

## ursa-agent (C4)

1. Gateway key (once): `gateway-admin --json generate --user ursa-agent --role Staff --college ITS
   --department "Research Computing" --pi "Chuck Forsyth" --pi-email <pi email> --lab "Research Computing"
   --budget 50 --tpm 1000000 --rpm 300 --duration 365d`, then pipe the `key` field straight into
   `gcloud secrets create ursa-agent-gateway-key --data-file=-` (never print it).
2. `agent/deploy.sh plan`, then `agent/deploy.sh apply`. It creates the `ursa-agent` service account
   (no cluster, no Vertex access), the session secret, builds and deploys
   `https://ursa-agent-<project number>.us-central1.run.app`.
3. Open the URL and sign in. The agent registers itself as a bifrost OAuth client on first use.
4. Since 0.3.0 the page is a read-only dashboard (SPEC section 20) with the chat in a drawer. Panels
   call bifrost tools directly with the signed-in person's token (no model, no gateway spend);
   staff panels appear for people with tier R2. Redeploy after a change: `agent/deploy.sh apply`
   (answer y to the deploy step; secrets and the service account already exist), then
   `curl <url>/health` shows the new version.

