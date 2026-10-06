echo "=== grep remote/push/prometheus errors in journal ==="
journalctl -u ddos-worker --no-pager | grep -iE 'remote|push|prometheus|401|403|error' | tail -40 || true
echo "=== first 50 lines of worker run ==="
journalctl -u ddos-worker --no-pager | head -50