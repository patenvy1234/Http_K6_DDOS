import azure.functions as func
import logging
import os
from datetime import datetime, timezone

from azure.identity import ManagedIdentityCredential
from azure.mgmt.resource import ResourceManagementClient

app = func.FunctionApp()

@app.timer_trigger(schedule="0 */5 * * * *", arg_name="timer", run_on_startup=False)
def cleanup_expired_rgs(timer: func.TimerRequest) -> None:
    """Delete ephemeral ddos resource groups whose auto-delete-at tag has elapsed."""
    subscription_id = os.environ["AZURE_SUBSCRIPTION_ID"]
    credential = ManagedIdentityCredential()
    client = ResourceManagementClient(credential, subscription_id)

    now = datetime.now(timezone.utc)
    deleted = 0
    skipped = 0

    for rg in client.resource_groups.list():
        tags = rg.tags or {}
        purpose = tags.get("purpose", "")

        # Only touch our ephemeral load-gen RGs.
        if purpose != "ddos-loadgen":
            continue

        # Never delete the persistent observability RG (tagged differently but
        # double-check by name prefix too).
        if rg.name.startswith("ddos-observability") or "observability" in rg.name:
            skipped += 1
            continue

        raw_ts = tags.get("auto-delete-at", "")
        if not raw_ts:
            logging.warning("RG %s has purpose=ddos-loadgen but no auto-delete-at tag — skipping", rg.name)
            skipped += 1
            continue

        try:
            deadline = datetime.fromisoformat(raw_ts.replace("Z", "+00:00"))
        except ValueError:
            logging.warning("RG %s has unparseable auto-delete-at=%r — skipping", rg.name, raw_ts)
            skipped += 1
            continue

        if now < deadline:
            remaining = int((deadline - now).total_seconds() / 60)
            logging.info("RG %s: deadline in %d min — keeping", rg.name, remaining)
            continue

        logging.warning(
            "RG %s: deadline %s has elapsed (now %s) — deleting",
            rg.name, deadline.isoformat(), now.isoformat(),
        )
        try:
            poller = client.resource_groups.begin_delete(rg.name)
            poller.result(timeout=30)  # wait up to 30 s; ARM may still be processing
            logging.info("RG %s deleted", rg.name)
            deleted += 1
        except Exception as exc:  # pylint: disable=broad-except
            logging.error("Failed to delete RG %s: %s", rg.name, exc)

    logging.info("cleanup_expired_rgs done: deleted=%d skipped=%d", deleted, skipped)
