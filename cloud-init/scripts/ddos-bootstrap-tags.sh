#!/usr/bin/env bash
# Derive per-instance tags (region, instance name) from IMDS and write them
# into /etc/ddos/tags.env so the systemd unit can pass them to k6 as --tag flags.
# Runs once during cloud-init.
set -euo pipefail

META="$(curl -fsSL --retry 10 --retry-delay 2 -H "Metadata: true" \
  "http://169.254.169.254/metadata/instance?api-version=2021-02-01")"

REGION="$(echo "${META}" | jq -r '.compute.location')"
VM_ID="$(echo "${META}" | jq -r '.compute.vmId')"
NAME="$(echo "${META}" | jq -r '.compute.name')"

mkdir -p /etc/ddos
cat > /etc/ddos/tags.env <<EOF
DDOS_REGION=${REGION}
DDOS_INSTANCE=${NAME}
DDOS_VM_ID=${VM_ID}
EOF

echo "ddos-bootstrap-tags: wrote /etc/ddos/tags.env (region=${REGION} name=${NAME})"
