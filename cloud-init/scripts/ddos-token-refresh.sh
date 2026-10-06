#!/usr/bin/env bash
# Refresh the Entra access token for k6's Prometheus remote_write output and
# atomically write it as a systemd EnvironmentFile that ddos-worker.service
# reads on (re)start.
#
# Entra tokens are valid for ~24h; refresh well below that.
set -euo pipefail

ENV_FILE="/etc/ddos/k6.env"
RESOURCE="https://monitor.azure.com"
REFRESH_INTERVAL="${DDOS_TOKEN_REFRESH_INTERVAL:-3000}"  # 50 minutes
REFRESH_JITTER="${DDOS_TOKEN_REFRESH_JITTER:-300}"       # +/- 5 minutes
READY_SENT=false

if ! [[ "${REFRESH_INTERVAL}" =~ ^[0-9]+$ ]] || [ "${REFRESH_INTERVAL}" -lt 60 ]; then
  echo "ddos-token-refresh: interval must be an integer >= 60" >&2
  exit 1
fi
if ! [[ "${REFRESH_JITTER}" =~ ^[0-9]+$ ]]; then
  echo "ddos-token-refresh: jitter must be a non-negative integer" >&2
  exit 1
fi

mkdir -p "$(dirname "${ENV_FILE}")"

while true; do
  if TOK="$(/usr/local/bin/ddos-imds-token "${RESOURCE}")" && [ -n "${TOK}" ]; then
    umask 077
    TMP="$(mktemp /etc/ddos/k6.env.XXXXXX)"
    cat > "${TMP}" <<EOF
K6_PROMETHEUS_RW_BEARER_TOKEN=${TOK}
EOF
    mv -f "${TMP}" "${ENV_FILE}"
    echo "ddos-token-refresh: wrote ${ENV_FILE} ($(date -u +%FT%TZ))"
    if [ "${READY_SENT}" = false ]; then
      if [ -n "${NOTIFY_SOCKET:-}" ]; then
        systemd-notify --ready --status="initial Azure Monitor token ready"
      fi
      READY_SENT=true
    fi
  else
    echo "ddos-token-refresh: IMDS token fetch failed; retrying in 30s" >&2
    sleep 30
    continue
  fi

  JITTER=0
  if [ "${REFRESH_JITTER}" -gt 0 ]; then
    JITTER=$((RANDOM % (2 * REFRESH_JITTER + 1) - REFRESH_JITTER))
  fi
  NEXT_REFRESH=$((REFRESH_INTERVAL + JITTER))
  if [ "${NEXT_REFRESH}" -lt 60 ]; then
    NEXT_REFRESH=60
  fi
  echo "ddos-token-refresh: next refresh in ${NEXT_REFRESH}s"
  sleep "${NEXT_REFRESH}"
done
