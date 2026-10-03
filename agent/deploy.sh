#!/usr/bin/env bash
# Deploy ursa-agent (Gemini agent: chat page + A2A) to Cloud Run.
#
#   agent/deploy.sh plan     # show what would be created (default; changes nothing)
#   agent/deploy.sh apply    # do it (asks before each billable or IAM change)
#
# The gateway key secret (ursa-agent-gateway-key) is created separately with
# gateway-admin (see docs/DEPLOY.md, "ursa-agent"), never by this script.
set -euo pipefail
cd "$(dirname "$0")"
MODE=${1:-plan}
PROJECT=${PROJECT:-ucr-ursa-major-hpc-cluster}
REGION=${REGION:-us-central1}
SERVICE=ursa-agent
SA=ursa-agent@${PROJECT}.iam.gserviceaccount.com
REPO=${REGION}-docker.pkg.dev/${PROJECT}/bifrost
VERSION=$(python3 -c "import tomllib;print(tomllib.load(open('pyproject.toml','rb'))['project']['version'])")
IMAGE=${REPO}/ursa-agent:v${VERSION}
NUM=$(gcloud projects describe "$PROJECT" --format='value(projectNumber)')
URL="https://${SERVICE}-${NUM}.${REGION}.run.app"
BIFROST="https://bifrost-mcp-${NUM}.${REGION}.run.app/mcp"

say()  { printf '\n== %s\n' "$*"; }
run()  { if [ "$MODE" = apply ]; then echo "+ $*"; "$@"; else echo "would run: $*"; fi; }
ask()  { [ "$MODE" = apply ] || return 0; read -r -p "$1 [y/N] " a; [ "$a" = y ] || { echo "skipped"; return 1; }; }
have() { "$@" >/dev/null 2>&1; }

say "Plan for $SERVICE ($IMAGE) at $URL"

say "1. Service account (no cluster access, no Vertex: model calls go through the AI gateway key)"
if have gcloud iam service-accounts describe "$SA" --project "$PROJECT"; then echo "ok   $SA"
else ask "create $SA?" && run gcloud iam service-accounts create ursa-agent --project "$PROJECT" --display-name "ursa-agent (no cluster access)"; fi

say "2. Secrets"
for s in ursa-agent-gateway-key ursa-agent-session-secret; do
  if have gcloud secrets describe "$s" --project "$PROJECT"; then echo "ok   $s"; else echo "MISSING $s"; fi
done
if [ "$MODE" = apply ]; then
  have gcloud secrets describe ursa-agent-session-secret --project "$PROJECT" || \
    { ask "create ursa-agent-session-secret (random)?" && openssl rand -base64 32 | tr -d '\n' | gcloud secrets create ursa-agent-session-secret --project "$PROJECT" --replication-policy automatic --data-file=-; }
  for s in ursa-agent-gateway-key ursa-agent-session-secret; do
    have gcloud secrets describe "$s" --project "$PROJECT" && gcloud secrets add-iam-policy-binding "$s" --project "$PROJECT" --member "serviceAccount:$SA" --role roles/secretmanager.secretAccessor >/dev/null && echo "access ok $s"
  done
fi

say "2b. Data bucket (sealed sign-ins, so a restart does not sign people out; private, own bucket, 1-day delete)"
BUCKET=${PROJECT}-ursa-agent-data
if have gcloud storage buckets describe "gs://$BUCKET"; then echo "ok   gs://$BUCKET"
else ask "create gs://$BUCKET?" && { run gcloud storage buckets create "gs://$BUCKET" --project "$PROJECT" --location "$REGION" \
  --uniform-bucket-level-access --public-access-prevention --soft-delete-duration=0;
  printf '{"rule":[{"action":{"type":"Delete"},"condition":{"age":1}}]}' > /tmp/ursa-agent-lifecycle.json;
  run gcloud storage buckets update "gs://$BUCKET" --lifecycle-file=/tmp/ursa-agent-lifecycle.json; }; fi
run gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" --member "serviceAccount:$SA" --role roles/storage.objectUser >/dev/null

say "3. Image"
run docker build -t "$IMAGE" .
run docker push "$IMAGE"

say "4. Cloud Run (public URL; every chat/A2A call needs a bifrost sign-in)"
ask "deploy $SERVICE?" && run gcloud run deploy "$SERVICE" --project "$PROJECT" --region "$REGION" --image "$IMAGE" \
  --service-account "$SA" --allow-unauthenticated --min-instances 0 --max-instances 1 --concurrency 20 \
  --cpu 1 --memory 1Gi --timeout 600 --execution-environment gen2 \
  --set-env-vars "AGENT_BASE_URL=$URL,BIFROST_MCP_URL=$BIFROST,URSA_AGENT_MODEL=gemini-3.8-flash,AGENT_DATA_DIR=/data" \
  --add-volume "name=data,type=cloud-storage,bucket=$BUCKET" --add-volume-mount "volume=data,mount-path=/data" \
  --set-secrets "GATEWAY_API_KEY=ursa-agent-gateway-key:latest,AGENT_SESSION_SECRET=ursa-agent-session-secret:latest"

say "Done ($MODE). $URL"
[ "$MODE" = apply ] && curl -fsS "$URL/health" && echo
