set -e
echo "=== journalctl ddos-worker tail ==="
journalctl -u ddos-worker --no-pager -n 40 || true
echo "=== k6 process ==="
pgrep -fa k6 || echo "no k6 process"
echo "=== status ==="
systemctl status ddos-worker --no-pager | head -20 || true