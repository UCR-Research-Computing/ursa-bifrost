# IAP + OS Login prototype

Reaches the Ursa Major login node as the signed-in user from plain Go, with only that user's
Google OAuth access token (no gcloud, no ssh binary, no service account). See
`docs/CLOUD_PLAN.md` section 2.1.

    ACCESS_TOKEN="$(gcloud auth print-access-token)" EMAIL="$(gcloud config get-value account)" go run .

It registers a 5-minute ed25519 key on the caller's OS Login profile, opens an IAP TCP tunnel
to port 22, runs `id -un`, `squeue --me` and `sinfo`, and deletes the key. Prototype only: the
host key is not pinned. Separate Go module so it does not affect the main build.
