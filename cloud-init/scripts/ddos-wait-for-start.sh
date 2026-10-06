#!/usr/bin/env bash
# Block until the synchronized start_at timestamp from profile.json arrives.
# All workers run this so they begin within ~1s of each other regardless of
# how staggered their VMSS provisioning was.
set -euo pipefail

PROFILE="/etc/ddos/profile.json"

if [ ! -f "${PROFILE}" ]; then
  echo "ddos-wait-for-start: ${PROFILE} missing" >&2
  exit 1
fi

START="$(jq -r '.start_at // empty' "${PROFILE}")"
if [ -z "${START}" ] || [ "${START}" = "null" ]; then
  echo "ddos-wait-for-start: no start_at in profile.json; starting immediately"
  exit 0
fi

NOW_EPOCH="$(date -u +%s)"
START_EPOCH="$(date -u -d "${START}" +%s)"
DELAY=$((START_EPOCH - NOW_EPOCH))

if [ "${DELAY}" -gt 0 ]; then
  echo "ddos-wait-for-start: sleeping ${DELAY}s until ${START}"
  sleep "${DELAY}"
else
  echo "ddos-wait-for-start: start_at ${START} already in the past (delay=${DELAY}s); starting immediately"
fi
