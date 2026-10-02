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
3. Clients: Create client, type **Web application**. Authorized redirect URI:
   `<service url>/oauth/google/callback` (add it after the first deploy prints the URL).
4. Put the client ID in the config (`server.google_client_id`). Store the client
   secret in Secret Manager: `gcloud secrets create bifrost-google-client-secret --data-file=-`.

## Deploy

```
cp deploy/config.example.yaml ~/bifrost-deploy/config.yaml    # fill in host_keys, client id, base_url
cp deploy/users.example.yaml  ~/bifrost-deploy/users.yaml     # the people allowed
cat > deploy/env <<EOF                                        # git-ignored
PROJECT=ucr-ursa-major-hpc-cluster
REGION=us-central1
CONFIG=$HOME/bifrost-deploy/config.yaml
USERS=$HOME/bifrost-deploy/users.yaml
EOF
deploy/deploy.sh plan      # read-only: shows what is missing
deploy/deploy.sh apply     # asks before each billable or IAM change
```

Cost when idle is zero: min-instances 0, a few MB in Cloud Storage, and Secret
Manager versions. While in use: one small Cloud Run instance (1 vCPU, 512 MiB).

## Connect a client

Hermes: `hermes mcp add ursa-cloud --url https://<service url>/mcp`. Hermes
discovers the OAuth server, registers itself, opens the browser for Google
sign-in, and stores bifrost's tokens (never the Google ones).

Sign out (deletes the stored Google token and SSH key):
`curl -X POST -H "Authorization: Bearer <token>" <url>/signout`, or remove
ursa-bifrost from https://myaccount.google.com/permissions. bifrost notices on
the next token refresh and ends the session.
