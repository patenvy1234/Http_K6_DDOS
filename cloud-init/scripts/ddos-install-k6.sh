#!/usr/bin/env bash
# Install a pinned k6 binary release from GitHub.
# Version pinned via /etc/ddos/runtime.env (DDOS_K6_VERSION).
set -euo pipefail

# shellcheck disable=SC1091
source /etc/ddos/runtime.env

K6_VERSION="${DDOS_K6_VERSION:-0.58.0}"
ARCH="$(uname -m)"
case "${ARCH}" in
  x86_64)  K6_ARCH="amd64" ;;
  aarch64) K6_ARCH="arm64" ;;
  *) echo "unsupported arch: ${ARCH}" >&2; exit 1 ;;
esac

URL="https://github.com/grafana/k6/releases/download/v${K6_VERSION}/k6-v${K6_VERSION}-linux-${K6_ARCH}.tar.gz"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

echo "ddos-install-k6: downloading ${URL}"
curl -fsSL --retry 10 --retry-delay 2 --retry-connrefused \
  --max-time 120 -o "${TMP}/k6.tgz" "${URL}"

tar -xzf "${TMP}/k6.tgz" -C "${TMP}"
install -m 0755 "${TMP}/k6-v${K6_VERSION}-linux-${K6_ARCH}/k6" /usr/local/bin/k6
/usr/local/bin/k6 version
