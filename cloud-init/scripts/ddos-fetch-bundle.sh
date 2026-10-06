#!/usr/bin/env bash
# Fetch the per-run bundle (profile.json + runner.js + lib/*) from the run's
# private blob container, authenticating with the VMSS user-assigned identity.
# Runs as ExecStartPre of ddos-worker.service.
set -euo pipefail

# shellcheck disable=SC1091
source /etc/ddos/runtime.env

DEST_DIR="/etc/ddos"
BUNDLE_URL="https://${DDOS_STORAGE_ACCOUNT}.blob.core.windows.net/${DDOS_BLOB_CONTAINER}/${DDOS_BUNDLE_BLOB}"
TMP="$(mktemp -d)"
STAGE="${TMP}/stage"
trap 'rm -rf "${TMP}"' EXIT
mkdir -p "${STAGE}"

echo "ddos-fetch-bundle: fetching ${BUNDLE_URL}"

TOK="$(/usr/local/bin/ddos-imds-token "https://storage.azure.com")"
if [ -z "${TOK}" ]; then
  echo "ddos-fetch-bundle: empty token from IMDS" >&2
  exit 1
fi

# Retry with backoff: the controller may still be uploading the bundle while
# the first VMSS instances reach this stage.
curl -fsSL --retry 60 --retry-delay 5 --retry-connrefused \
  --max-time 600 \
  -H "x-ms-version: 2023-11-03" \
  -H "Authorization: Bearer ${TOK}" \
  -o "${TMP}/bundle.tar.gz" \
  "${BUNDLE_URL}"

tar -xzf "${TMP}/bundle.tar.gz" \
  --directory "${STAGE}" \
  --no-same-owner \
  --no-same-permissions

for REQUIRED in runner.js profile.json lib/profile.js lib/transport.js lib/metrics.js; do
  if [ ! -s "${STAGE}/${REQUIRED}" ]; then
    echo "ddos-fetch-bundle: bundle is missing ${REQUIRED}" >&2
    exit 1
  fi
done
jq -e '
  (.run_id | type == "string" and length > 0) and
  (.target_rps | type == "number" and . > 0) and
  (.endpoints | type == "array" and length > 0)
' "${STAGE}/profile.json" >/dev/null

cp -a "${STAGE}/." "${DEST_DIR}/"
echo "ddos-fetch-bundle: extracted to ${DEST_DIR}"
ls -la "${DEST_DIR}"
