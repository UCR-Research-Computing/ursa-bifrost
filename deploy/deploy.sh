#!/usr/bin/env bash
# Deploy the hosted ursa-bifrost MCP server to Cloud Run.
#
#   deploy/deploy.sh plan     # show what would be created/changed (default; changes nothing)
#   deploy/deploy.sh apply    # do it (asks before each billable or IAM change)
#
# Needs: gcloud logged in as a project owner, docker. Reads deploy/env (not in git):
#   PROJECT=ucr-ursa-major-hpc-cluster  REGION=us-central1
#   CONFIG=path/to/config.yaml  USERS=path/to/users.yaml
#   CLIENT_JSON=path/to/oauth-client.json   (downloaded when creating the Web OAuth client; see docs/DEPLOY.md)
set -euo pipefail
cd "$(dirname "$0")/.."
MODE=${1:-plan}
[ -f deploy/env ] || { echo "deploy/env missing (see the header of this script)"; exit 2; }
# shellcheck disable=SC1091
. deploy/env
: "${PROJECT:?}" "${REGION:?}" "${CONFIG:?}" "${USERS:?}"
SERVICE=bifrost-mcp
SA=bifrost-mcp@${PROJECT}.iam.gserviceaccount.com
REPO=${REGION}-docker.pkg.dev/${PROJECT}/bifrost
BUCKET=${PROJECT}-bifrost-data
VERSION=$(git describe --tags --always --dirty)
IMAGE=${REPO}/bifrost:${VERSION}

say()  { printf '\n== %s\n' "$*"; }
run()  { if [ "$MODE" = apply ]; then echo "+ $*"; "$@"; else echo "would run: $*"; fi; }
ask()  { [ "$MODE" = apply ] || return 0; read -r -p "$1 [y/N] " a; [ "$a" = y ] || { echo "skipped"; return 1; }; }
have() { "$@" >/dev/null 2>&1; }

say "Plan for $SERVICE in $PROJECT ($REGION), image $IMAGE"

say "1. APIs"
for api in run.googleapis.com artifactregistry.googleapis.com secretmanager.googleapis.com; do
  if gcloud services list --enabled --project "$PROJECT" --format='value(config.name)' | grep -qx "$api"; then echo "ok   $api"
  else ask "enable $api?" && run gcloud services enable "$api" --project "$PROJECT"; fi
done

say "2. Service account (no cluster access: it never SSHes; it reads secrets and the data bucket)"
if have gcloud iam service-accounts describe "$SA" --project "$PROJECT"; then echo "ok   $SA"
else ask "create $SA?" && run gcloud iam service-accounts create bifrost-mcp --project "$PROJECT" --display-name "ursa-bifrost MCP server (no cluster access)"; fi

say "3. Artifact Registry repo"
if have gcloud artifacts repositories describe bifrost --location "$REGION" --project "$PROJECT"; then echo "ok   $REPO"
else ask "create repo bifrost?" && run gcloud artifacts repositories create bifrost --repository-format docker --location "$REGION" --project "$PROJECT"; fi

say "4. Data bucket (sealed sessions, keys, ledgers; AES-GCM inside, private, versioned)"
if have gcloud storage buckets describe "gs://$BUCKET"; then echo "ok   gs://$BUCKET"
else ask "create gs://$BUCKET?" && { run gcloud storage buckets create "gs://$BUCKET" --project "$PROJECT" --location "$REGION" --uniform-bucket-level-access --public-access-prevention;
  run gcloud storage buckets update "gs://$BUCKET" --versioning; }; fi
run gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" --member "serviceAccount:$SA" --role roles/storage.objectUser

say "5. Secrets (values never printed)"
for s in bifrost-secret-key bifrost-google-client-secret bifrost-config bifrost-users; do
  if have gcloud secrets describe "$s" --project "$PROJECT"; then echo "ok   $s"
  else echo "MISSING $s"; fi
done
if [ "$MODE" = apply ]; then
  have gcloud secrets describe bifrost-secret-key --project "$PROJECT" || \
    { ask "create bifrost-secret-key (32 random bytes)?" && openssl rand -base64 32 | tr -d '\n' | gcloud secrets create bifrost-secret-key --project "$PROJECT" --data-file=-; }
  if ! have gcloud secrets describe bifrost-google-client-secret --project "$PROJECT"; then
    if [ -n "${CLIENT_JSON:-}" ] && [ -f "$CLIENT_JSON" ]; then
      ask "create bifrost-google-client-secret from $CLIENT_JSON (value not printed)?" && \
        python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['web']['client_secret'],end='')" "$CLIENT_JSON" | \
        gcloud secrets create bifrost-google-client-secret --project "$PROJECT" --replication-policy automatic --data-file=-
    else
      echo "set CLIENT_JSON in deploy/env to the downloaded OAuth client JSON, then apply again"
    fi
  fi
  if [ -n "${CLIENT_JSON:-}" ] && [ -f "$CLIENT_JSON" ]; then
    cid=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['web']['client_id'])" "$CLIENT_JSON")
    grep -q "$cid" "$CONFIG" || echo "WARNING: $CONFIG server.google_client_id does not match $CLIENT_JSON"
  fi
  ask "upload $CONFIG as a new version of bifrost-config?" && { have gcloud secrets describe bifrost-config --project "$PROJECT" || gcloud secrets create bifrost-config --project "$PROJECT" --replication-policy automatic; gcloud secrets versions add bifrost-config --project "$PROJECT" --data-file="$CONFIG"; }
  ask "upload $USERS as a new version of bifrost-users?" && { have gcloud secrets describe bifrost-users --project "$PROJECT" || gcloud secrets create bifrost-users --project "$PROJECT" --replication-policy automatic; gcloud secrets versions add bifrost-users --project "$PROJECT" --data-file="$USERS"; }
  for s in bifrost-secret-key bifrost-google-client-secret bifrost-config bifrost-users; do
    have gcloud secrets describe "$s" --project "$PROJECT" && gcloud secrets add-iam-policy-binding "$s" --project "$PROJECT" --member "serviceAccount:$SA" --role roles/secretmanager.secretAccessor >/dev/null
  done
fi

say "6. Build and push the image"
run docker build --build-arg VERSION="$VERSION" -t "$IMAGE" .
run gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet
run docker push "$IMAGE"

say "7. Deploy Cloud Run (public URL; every MCP request needs a bifrost token from Google sign-in)"
ask "deploy $SERVICE?" && run gcloud run deploy "$SERVICE" --project "$PROJECT" --region "$REGION" --image "$IMAGE" \
  --service-account "$SA" --allow-unauthenticated --min-instances 0 --max-instances 1 --concurrency 40 \
  --cpu 1 --memory 512Mi --timeout 900 --execution-environment gen2 \
  --set-secrets "BIFROST_SECRET_KEY=bifrost-secret-key:latest,BIFROST_GOOGLE_CLIENT_SECRET=bifrost-google-client-secret:latest,/config/config.yaml=bifrost-config:latest,/users/users.yaml=bifrost-users:latest" \
  --add-volume "name=data,type=cloud-storage,bucket=$BUCKET" --add-volume-mount "volume=data,mount-path=/data"

URL="https://${SERVICE}-$(gcloud projects describe "$PROJECT" --format='value(projectNumber)')-${REGION}.run.app"
say "Done ($MODE). Service URL: $URL (deterministic: set it as server.base_url and add $URL/oauth/google/callback to the OAuth client BEFORE the first deploy)."
[ "$MODE" = apply ] && curl -fsS "$URL/health" && echo
