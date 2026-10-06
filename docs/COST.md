# Cost Reference — Azure-backed K6 Load Generator

Estimates for a **1-hour sustained run** at each RPS tier.  
All numbers are approximations based on Azure East US pricing (~2026).  
Actual spend varies with spot eviction rates, region mix, and request/response sizes.

---

## Assumptions

| Item | Value |
|---|---|
| VM SKU | `Standard_F4s_v2` (4 vCPU, 8 GB RAM) — default for all built-in profiles |
| Throughput per VM (target / planning) | **5,000 RPS configured**; the table conservatively plans at ~2,500 observed RPS per F4s_v2. Controller compensates by scaling out when observed RPS remains below target. |
| Spot price — F4s_v2 | ~$0.035 / VM / hr (~75% off on-demand ~$0.169) |
| Standard Public IP | $0.004 / IP / hr (one per VM, per-VM egress model — no NAT Gateway) |
| Egress (Azure → Internet) | $0.087 / GB (first 10 TB/month) |
| Azure Managed Prometheus | < $0.10 / hr (ingestion at these scales, negligible) |
| Azure Managed Grafana (Standard SKU) | ~$0.10 / hr (shared observability RG, fixed) |
| Storage (bundle + logs) | < $0.01 / run (negligible) |
| Cleanup Function (FC1) | < $0.01 / hr at idle |

> **Egress note**: Egress here means the **HTTP request bytes** leaving Azure from the K6 VMs
> toward your target. Response bytes coming back into Azure are **free (ingress)**.  
> — *Lean request*: GET with minimal headers, no body — ~400 B/request  
> — *Heavy request*: POST with ~2 KB body — ~2 300 B/request

---

## Cost Table — 1-Hour Test

VM counts below are the **scale-out steady state** (~2,500 RPS empirical per F4s_v2),
not the initial fleet (initial = `⌈targetRPS / 5,000⌉`). Compute cost uses the conservative
count since that's what runs for most of the test wall-clock.

| RPS Target | VMs (F4s_v2, empirical) | Regions | VM Compute | Public IPs | Egress (lean) | Egress (heavy) | **Total / hr (lean)** | **Total / hr (heavy)** |
|---|---|---|---|---|---|---|---|---|
| **50 k** | 20 | 1 | $0.70 | $0.08 | $5.83 | $33.6 | **~$7** | **~$34** |
| **100 k** | 40 | 2 | $1.40 | $0.16 | $11.67 | $67.1 | **~$13** | **~$69** |
| **200 k** | 80 | 4 | $2.80 | $0.32 | $23.33 | $134.2 | **~$26** | **~$137** |
| **500 k** | 200 | 8 | $7.00 | $0.80 | $58.34 | $335.5 | **~$66** | **~$343** |
| **1 M** † | 400 | 16 † | $14.00 | $1.60 | $116.68 | $671.0 | **~$132** | **~$687** |

> † **1M RPS requires either a quota increase or more regions.** The current
> subscription (`<your-subscription>`) has 100 vCPU/region across **9 regions** where
> F4s_v2 is offered = max **225 F4s_v2 VMs** (theoretical configured ceiling
> ≈1.125 M configured RPS at 5,000 RPS/VM, ~562 k at the conservative 2,500 planning rate).
> To **empirically** sustain 1 M RPS you need either:
> - **Option A** — Increase quota to 200 vCPU/region in the same 9 regions
>   (9 × 50 F4s_v2 × 2,500 RPS ≈ 1.12 M ✓)
> - **Option B** — Open quota in 7+ additional regions (16 regions × 25 F4s_v2 × 2,500 RPS ≈ 1 M ✓)
>
> Submit quota increases via **Azure Portal → Subscriptions → Usage + Quotas → Request Increase**
> for `standardFSv2Family` in each target region.
>
> After any quota change, re-run `ddos refresh-quota` to refresh
> `region-quota.yaml` so the runner picks up the new headroom.

---

## Egress Math (how the table was computed)

```
egress_GB = RPS × bytes_per_request × 3600 s ÷ 1 073 741 824
egress_cost = egress_GB × $0.087
```

| RPS | Lean (400 B) | Heavy (2 300 B) |
|---|---|---|
| 50 k | 67 GB | 386 GB |
| 100 k | 134 GB | 771 GB |
| 200 k | 268 GB | 1 542 GB |
| 500 k | 671 GB | 3 856 GB |
| 1 M | 1 341 GB (1.3 TB) | 7 712 GB (7.5 TB) |

All values are below the 10 TB/month tier boundary for a single 1-hour run, so the
flat rate of $0.087/GB applies throughout.

---

## Cost-Reduction Tips

| Technique | Impact |
|---|---|
| Spot VMs (already default) | ~75% compute cost reduction vs on-demand |
| `discardResponseBodies: true` (already default) | Eliminates response-side processing; also reduces ingress on VMs |
| `--no-summary` flag (already default) | Prevents in-process memory growth; no cost impact but prevents OOM |
| Short test durations | Egress dominates at high RPS — keep tests to minutes, not hours |
| Lean request payloads | Biggest lever on egress cost; use GET/HEAD where possible |
| Run during off-peak hours | Spot availability and pricing improves evenings/weekends |

---

## CLI Budget Guard

Use the profile-aware estimator before deployment:

```powershell
Copy-Item examples\attack-simple.yaml smoke.local.yaml
# Replace example.com with an explicitly authorized target first.
.\ddos.exe validate smoke.local.yaml --json --max-cost 10
.\ddos.exe run smoke.local.yaml --max-cost 10
```

The guard assumes `maxVMCount` for the full lifecycle TTL and includes weighted
request bodies, connection-pattern overhead, VM compute, public IPs, and outbound
traffic. Traffic is sized at the larger of the requested RPS and
`maxVMCount * 5,000`, for the entire TTL, because workers have a fixed local rate
and scaled-out workers can exceed the requested aggregate goal. JSON output
includes `maxTrafficRps` and `trafficSeconds` to make this assumption visible.

Compute uses the on-demand reference rate even for Spot VMs: the deployment's
`maxPrice: -1` permits Spot prices up to on-demand. This differs intentionally
from the illustrative discounted Spot table above.

This is a conservative planning estimate, **not a hard billing cap or a live
Azure quote**. Regional price differences, protocol overhead, deletion delays,
persistent observability, and target-side charges can change actual spend.
Use Azure budgets and verify cleanup independently. `run` defaults to a $200
planning ceiling; `--max-cost 0` disables it explicitly. Negative, NaN, and
infinite limits are rejected.

## Existing Attack Profiles

| Profile | RPS | Fleet (auto-derived @ 5,000 RPS/VM target) | Est. cost / 1 hr (lean) |
|---|---|---|---|
| `examples/attack-simple.yaml` | 5 000 | 1 × F4s_v2 / 1 region (auto) | < $1 |
| `examples/attack-200k.yaml` | 200 000 | 40 × F4s_v2 across 4 regions (auto, 10/region), max 84 total | ~$26 |
| `examples/attack-1m.yaml` | 1 000 000 | 200 × F4s_v2 auto-spread across available regions, max 200 total† | ~$132 lean; much higher with its weighted 32 KiB POST |

† `attack-1m.yaml` uses `regions: []`, so the quota cache auto-expands across
all available regions. Its initial 200 VMs fit only when the selected regions
have at least 800 Fsv2-family vCPUs in aggregate, and it leaves no scale-out
headroom under `maxVMCount: 200`.
