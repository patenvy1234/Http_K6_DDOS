# ddos — Azure-backed K6 load generator (target: 1M RPS)

A CLI tool that deploys K6 worker fleets across Azure regions, drives traffic against a set of
configured endpoints, streams metrics to Azure Monitor managed Prometheus, and tears everything
down when the run ends.

> **Use for load-testing endpoints you own.** This tool is built for synthetic load against your
> own services and benchmark targets. Don't point it at infrastructure you don't control.

> **AI agents driving this repo: read [the root AGENTS.md](../AGENTS.md) first.**
> It defines the authorization, budget, build, and activation contract.

## Quick start

```powershell
# 1. One-time: build the CLI
go build -o ddos.exe ./cmd/ddos

# 2. One-time: log in & deploy persistent observability stack
az login
.\ddos.exe init-observability --subscription <sub-id> --location eastus

# 3. One-time: deploy the subscription-level cleanup safety net
.\ddos.exe init-safety-net --subscription <sub-id>

# 4. One-time: cache region/SKU/quota info into region-quota.yaml
.\ddos.exe refresh-quota

# 5. Per run: copy a template, replace every URL with an authorized target,
# approve a budget, then validate and run the git-ignored local profile
Copy-Item examples/attack-simple.yaml smoke.local.yaml
# Edit smoke.local.yaml before continuing; example.com is rejected.
.\ddos.exe validate smoke.local.yaml --json --max-cost 10
.\ddos.exe run smoke.local.yaml --max-cost 10

# 6. Watch dashboards live (opens the Grafana URL with this run pre-filtered)
.\ddos.exe observe
```

## Built-in attack profiles

| Profile | RPS | Fleet (auto-derived @ 5,000 RPS/VM) | Notes |
|---|---|---|---|
| `examples/attack-simple.yaml` | 5 000 | 1 × F4s_v2 / 1 region | Template; replace its placeholder target before use. |
| `examples/attack-200k.yaml`   | 200 000 | ⌈200 000 / 5 000⌉ = **40 VMs total** across 4 regions = 10/region, max 84 | Configured goal, not a guaranteed achieved rate. |
| `examples/attack-1m.yaml`     | 1 000 000 | ⌈1 000 000 / 5 000⌉ = **200 VMs total** auto-spread across available regions, max 200 | Replace target URLs and approve a sufficient budget before use. |

## Architecture

- **Controller**: Go CLI on your laptop. Deploys Bicep, allocates regions via the static quota
  cache (`region-quota.yaml`), monitors AMW via Prometheus query API, and **scales VMSS
  horizontally** after observed RPS remains below 95% target for two polls.
- **Workers**: Ubuntu 24.04 spot VMs (`Standard_F4s_v2` 4 vCPU, default) in a
  VM Scale Set per region. cloud-init installs K6 from the GitHub release, configures sysctl +
  systemd, and runs `k6 run --no-summary` to push metrics via remote-write.
- **Egress**: Each VM gets its own Standard SKU Public IPv4 address. No NAT Gateway. See
  `docs/AGENTS.md` §1 for the rationale.
- **Auth (worker → metrics)**: User-assigned managed identity (UAMI) per run. A sidecar service
  refreshes an Entra Bearer token from IMDS every ~50 minutes and writes it to `/etc/ddos/k6.env`.
- **Auth (worker → bundle blob)**: Same UAMI; `ddos-fetch-bundle.sh` requests a
  `https://storage.azure.com` token from IMDS.
- **Observability**: Azure Monitor Workspace (managed Prometheus) + Azure Managed Grafana
  (Standard SKU), provisioned once in the persistent `ddos-observability` RG.
- **Cleanup**: Three layers — signal handler → runner `defer` (uses `context.Background()` so it
  survives Ctrl+C) → subscription-level Azure Function (Flex Consumption FC1, every 5 min, deletes
  RGs whose `auto-delete-at` tag is in the past).

## Scaling logic (in 30 seconds)

The watch goroutine polls AMW every ~30 s after warmup. It scales out when
`observed_RPS < 95% target` for **2 consecutive ticks**. A concurrent
`k6_dropped_iterations_rate > 5/s` signal increases the added headroom; dropped
iterations alone do not scale a fleet that is already meeting its target.

Growth strategy is **fresh-region-first**: round-robin via
`quotacache.PlanAllocation` (reads `region-quota.yaml`), excluding already-used and
known-bad regions. Only when all eligible regions are claimed does the runner grow
existing VMSS (per-VMSS hard cap = 200 in Bicep). 90 s cooldown between scale actions.

Spot evictions are handled by a parallel goroutine that re-scales each VMSS back to its
target capacity. Checks are scheduled every 30 seconds; Azure completion time varies.

Initial sizing honors explicit per-region counts. Auto sizing may select fewer
eligible regions to avoid rounding above `maxVMCount`. Initial deployment failure
aborts the run and initiates cleanup rather than redeploying over a partial fleet.
Uncertain scale operations reserve capacity until cleanup, so failures cannot
silently free slots for more VMs. Watchdog operations are canceled and joined
before resource-group deletion starts.

## Resource constraints

| Setting | Value |
|---|---|
| Per-VM RPS target (fixed) | **5,000 RPS** (constant `FixedPerVMRPS` in `profilegen.go`) |
| Fleet sizing | **Auto-derived** at 5,000 RPS/VM, selecting a uniform per-region count that fits the total ceiling; explicit initial counts are honored. |
| Default worker SKU | **`Standard_F4s_v2`** (4 vCPU, 8 GB RAM). `Standard_F8s_v2` (8 vCPU) is also allowlisted but not the default. |
| Per-region vCPU quota (subscription `<your-subscription>` snapshot) | **100** (→ 25 F4s_v2 per region) |
| Regions where F4s_v2 is offered (this sub) | **9** (australiaeast, eastus2, northeurope, southcentralus, southeastasia, uksouth, westeurope, westus2, westus3) |
| Purchase model | **Spot** (controller auto-replaces on eviction) |
| Per-VMSS hard cap | **200 VMs** (set by `@maxValue(200)` in `modules/region.bicep`) |
| Per-fleet hard cap | **500 VMs** (validated against `target.rps × 1/(vCPU × 15 000 RPS/vCPU)`) |
| Effective sub ceiling (today) | 9 regions × 25 F4s_v2 × 5 000 RPS ≈ **1.125 M configured RPS** at full quota |

## Per-test artifacts

Every `ddos run` creates a directory `runs/<runName>-<suffix>/`:
- `run.log` — full timestamped transcript (tee'd from stdout)
- `profile.yaml` — snapshot of the source profile used

These are git-ignored (`runs/` is in `.gitignore`).

## Layout

```
cmd/ddos/                   # Go CLI (cobra)
internal/                   # Controller logic (azure, monitor, orchestrator, quotacache, ...)
infra/bicep/                # Bicep templates (observability, per-run RG, cleanup function)
infra/grafana/              # Grafana dashboards (overview, per-endpoint)
infra/function/             # Safety-net Python timer Function
k6/                         # K6 entrypoint + lib + scenarios
cloud-init/                 # Worker bootstrap (k6, systemd, token refresh)
examples/                   # Sample attack.yaml profiles
region-quota.yaml           # Static quota cache (regenerate via `ddos refresh-quota`)
runs/                       # Per-test logs + profile snapshots (gitignored)
docs/                       # All documentation (this folder)
```

## Commands

| Command | Purpose |
|---|---|
| `ddos init-observability` | One-time: deploy AMW + DCE + DCR + Managed Grafana. |
| `ddos init-safety-net` | One-time: deploy subscription-level cleanup Azure Function (FC1). |
| `ddos refresh-quota` | Regenerate `region-quota.yaml` (regions, SKU availability, quota). |
| `ddos refresh-sku-cache` | (Optional fallback) Refresh `~/.ddos/sku-availability.json`. |
| `ddos validate <file>` | Validate and estimate maximum cost; supports `--json` and `--max-cost` without Azure calls. |
| `ddos run <file>` | Deploy fleet, sync-start workers, drive load, auto-scale, clean up. |
| `ddos destroy <run-id-or-rg>` | Force-delete a run RG (recovery only; refuses `ddos-observability`). |
| `ddos observe` | Open Azure Managed Grafana in your default browser. |
| `ddos doctor` | Verify Azure login, observability stack, and tool readiness. |

## Documentation

| File | Audience | Content |
|---|---|---|
| [README.md](./README.md) | New users | This file — quick start + overview |
| [AGENTS.md](./AGENTS.md) | AI coding agents | Step-by-step recipes per user intent + failure-mode triage |
| [FLOW.md](./FLOW.md) | Engineers debugging | Component-by-component, phase-by-phase trace of every file |
| [COST.md](./COST.md) | Capacity planners | Cost estimates for 50k–1M RPS × 1-hour runs |
