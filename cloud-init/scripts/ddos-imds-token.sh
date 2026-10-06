#!/usr/bin/env bash
# Fetch an Entra access token from the local IMDS endpoint for the given resource.
#
# Usage: ddos-imds-token [resource]
#   resource: default https://monitor.azure.com (k6 remote_write to AMW)
#             https://storage.azure.com for blob downloads
set -euo pipefail

RESOURCE="${1:-https://monitor.azure.com}"
API_VERSION="2018-02-01"

curl -fsSL \
  --retry 10 --retry-delay 2 --retry-connrefused \
  --max-time 10 \
  -H "Metadata: true" \
  "http://169.254.169.254/metadata/identity/oauth2/token?api-version=${API_VERSION}&resource=${RESOURCE}" \
  | jq -er '.access_token | select(type == "string" and length > 0)'
