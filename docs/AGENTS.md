# AGENTS.md — Briefing for AI agents driving this repo

> **You are an AI agent with no prior context for this repo.** Read this file
> top-to-bottom *before* running any command. It contains everything you need
> to execute load tests safely, interpret results, and report intelligently
> back to the user.

---

## 1. What this repo is (in 60 seconds)

`ddos` is a Go CLI that orchestrates a **K6 load-generation fleet on Azure**.

- **Target**: HTTP endpoints the user owns. Default endpoint in
  `examples/attack-simple.yaml`:
  `https://example.com/apc/trans.gif`
- **Generator**: K6, installed on Ubuntu 24.04 spot VMs (`Standard_F4s_v2`
  4 vCPU, default — `Standard_F8s_v2` 8 vCPU also allowlisted but not used by
  any built-in profile) in one VMSS per region.
- **Egress**: Each VM gets its own Standard SKU **per-VM public IPv4**
  address. No NAT Gateway. (Removed because we have no source-IP allowlist
  requirement; per-VM IPs avoid the NAT data-processing fee and look like
  distributed sources to the target.)
- **Metrics**: K6 streams native HTTP metrics (`k6_http_reqs_total`,
  `k6_http_req_duration` …) via Prometheus remote-write to **Azure Monitor
  Workspace (AMW) managed Prometheus**. Operator queries the same AMW.
- **Visualization**: Azure Managed Grafana (URL from `~/.ddos/config.yaml`).
- **Cleanup**: Always automatic — graceful exit, Ctrl+C, or the
  subscription-level safety-net Azure Function (Flex Consumption FC1, runs
  every 5 min, deletes RGs past their `auto-delete-at` tag).

Persistent resources (deployed once, shared across runs):
- RG `ddos-observability` — AMW + DCE + DCR + Grafana
- RG `ddos-safety-net`   — cleanup timer Function (FC1)

Per-run resources (created and torn down on each `ddos run`):
- RG `ddos-<runName>-<8charSuffix>` — UAMI + storage + VMSS per region

---

## 2. The ONE command you'll usually run

```powershell
go build -o ddos.exe ./cmd/ddos
.\ddos.exe doctor
.\ddos.exe validate <authorized-profile>.local.yaml --json --max-cost <approved-usd>
.\ddos.exe run <authorized-profile>.local.yaml --max-cost <approved-usd>
```

The run can take 5–30 minutes. Keep it in the foreground and let the command
finish so Ctrl+C, cleanup, and final status remain attached to the controller.
Before running, verify every target URL is user-owned or explicitly authorized;
all checked-in examples contain placeholders and must be copied and edited.

The tool prints **timestamped log lines** (HH:MM:SS) for every milestone:
foundation deploy → bundle upload → preflight → fleet deploy → first observed
RPS → scale events → spot evictions → cleanup. It also tees everything to
`runs\<runName>-<suffix>\run.log` and saves the source profile as
`runs\<runName>-<suffix>\profile.yaml`.

---

## 3. Existing attack profiles & when to use each

| Profile | RPS target | Pattern | Fleet (auto-derived @ 5,000 RPS/VM) | Use when user says … |
|---|---|---|---|---|
| `examples\attack-simple.yaml` | 5 000 | `keepalive-burst` | 1 × F4s_v2 in 1 region | "5k test", "sample run", "smoke test", "quick test" |
| `examples\attack-200k.yaml` | 200 000 | `keepalive-burst` | 40 × F4s_v2 across 4 regions (10/region), max 84 | "200k test", "200k attack" |
| `examples\attack-1m.yaml` | 1 000 000 | `keepalive-burst` | 200 × F4s_v2 auto-spread across available regions, max 200 | "1M test", "full attack", "max attack" |

> Every example references `https://example.com/...`. Copy the selected file to
> a git-ignored `*.local.yaml`, replace every URL, and get explicit authorization
> before running any profile.

### Sizing rules of thumb (subscription `<your-subscription>` snapshot: 100 vCPU/region)

- **Fixed per-VM target**: **5,000 RPS** (`profilegen.FixedPerVMRPS`). The
  K6 profile is generated with `target_rps = 5000` regardless of fleet size.
  The auto-scaler adds VMs whenever observed RPS remains below 95% of target.
- **Auto-derived fleet count**: `totalVMs = ⌈targetRPS / 5000⌉`,
  `vmPerRegion = ⌈totalVMs / len(regions)⌉`. Omit `initialVMCount` from the
  YAML (or set it to `0`) to let the controller derive both numbers.
- **Per-region capacity for F4s_v2 (4 vCPU)**: 25 VMs max
- **Regions where F4s_v2 is offered today (per `region-quota.yaml`)**:
  `australiaeast`, `eastus2`, `northeurope`, `southcentralus`,
  `southeastasia`, `uksouth`, `westeurope`, `westus2`, `westus3` (9 regions)
- **Hard configured ceiling today**: 9 regions × 25 F4s_v2 × 5,000 RPS ≈
  **1.125 M RPS** before accounting for target latency and worker saturation.

> ⚠️ **`maxVMCount` is TOTAL** across all regions (the scale-out budget).
> If you DO set `initialVMCount` manually, it is **PER REGION** — agents trip
> on this. Easiest path: omit `initialVMCount` and let the runner auto-derive.

---

## 4. Reading the run output (what to surface to the user)

```
[07:11:28] run attack-200k (suffix=8e29b297) starting        ← suffix = runId tag
[07:11:28] log file: ...\runs\attack-200k-8e29b297\run.log    ← all output tee'd here
[07:12:58] preflight: resolving Standard_F4s_v2 from quota cache ...
[07:12:58] ✓ all requested regions available: ...             ← OR "regions resolved: X (was: Y)" if fallback
[07:17:10] ✓ fleet deployed: ddos-...-eastus2-vmss, ...
[07:17:10] start at: 2026-05-26T07:16:28Z                     ← synchronized start across all VMs
[07:17:10] opening Grafana: https://ddos-grafana-...
[07:19:30] observed 198451 RPS / target 200000 RPS (99.2%)    ← polled every ~30s after warmup
[07:23:58] scaling out to fresh region northeurope (capacity=1) to close 1549 RPS deficit
[07:36:14] spot eviction: ddos-...-eastus2-vmss live=6 target=7 → scaling to 7
[07:46:00] ✓ test duration elapsed, initiating cleanup
[07:46:27] ✓ delete initiated for ddos-attack-200k-8e29b297
```

**Report to user**:
1. **Achieved RPS** = average of observed values (count the lines, eyeball avg)
2. **% of target** = avg / target × 100
3. **Scale events** if any (which fresh regions were added)
4. **Spot evictions** if any (eviction watcher handles them automatically)
5. **Cleanup status** — "RG delete initiated ✓"
6. **Log location** — `runs\<runName>-<suffix>\`

If user asks for analysis: read `runs\<runName>-<suffix>\run.log` (full
transcript persists after test ends).

---

## 5. Scaling logic (when an agent debugs "why didn't it hit target")

The watch loop (runs every ~30s after warmup):
1. Query AMW for **observed RPS**: `sum(rate(k6_http_reqs_total{runid="<suffix>"}[30s]))`
2. Query AMW for **dropped iter/s**: indicates k6 saturation (CPU,
   port-exhaustion, conntrack). Threshold: `5.0 iter/s`.
3. **Decide to scale** when RPS is below 95% target for **2 consecutive ticks**.
  Concurrent dropped iter/s above 5.0 increases the added headroom, but does
  not scale a fleet that is already meeting its target.
4. **Spread to fresh region first** (round-robin via `quotacache.PlanAllocation`
   reading `region-quota.yaml`). Already-used and `excludedRegions` are skipped.
5. Only when **all eligible regions are used** does it scale up existing VMSS
   (cap = 200 per VMSS in Bicep).
6. Per-VMSS eviction watcher runs in parallel: if `live < target`, scales
   VMSS back up to `target` (replaces evicted spot VMs).
7. **MaxVMCount cap** is the fleet ceiling — beyond this the runner logs
   "fleet at MaxVMCount" and stops growing.

**Cooldown**: 90 s between scale actions to avoid flapping.

---

## 6. K6 internals (relevant when debugging "VM saturated at low RPS")

- **Scenario**: always `constant-arrival-rate` (deterministic RPS).
- **VU budget**: pattern-specific latency headroom. Keepalive pre-allocates
  `ceil(perVMRPS / 40) + 50` and grows to `pre × 8` (cap 5,000).
  New-connection runs pre-allocate `ceil(perVMRPS / 30) + 200` and grow to
  `pre × 3` (cap 50,000).
  Hitting the VU ceiling produces dropped iterations so the controller adds
  workers instead of allowing one 8 GB VM to grow without bound.
- **Connection reuse**: `keepalive-burst` uses shared HTTP/1.1 pool (CRITICAL
  for high RPS — without it ephemeral ports exhaust ~28k conns/min).
- **Response bodies**: always discarded (`discardResponseBodies: true` +
  `responseType: 'none'`). Saves egress + memory.
- **Summary**: **DISABLED via `--no-summary` flag** (cloud-init/units/
  ddos-worker.service). Trend sinks would accumulate 80M+ samples per run
  otherwise.
- **Metrics tag**: every sample is tagged `runid=<suffix>` (cloud-init
  injects via `--tag runid=$DDOS_RUN_ID`) so different runs don't collide
  in AMW.

---

## 7. Static quota file: `region-quota.yaml`

This is the **single source of truth** for available regions + SKU
availability + quota slots in the current subscription. Regenerated only
when user runs:

```powershell
.\ddos.exe refresh-quota
```

(takes ~30 s — does **one** global `az vm list-skus --all` plus parallel
per-region `az vm list-usage`).

Each region entry (real shape — not made-up):

```yaml
regions:
  eastus2:
    families:
      standardFSv2Family:
        limit: 100
        currentValue: 0
    skus:
      Standard_F4s_v2: true
      Standard_F8s_v2: true
```

The runner's preflight reads this **before deploying** and prunes regions
where the SKU is unavailable or family quota is exhausted. Log lines:

- `✓ all requested regions available: ...` → no fallback
- `✓ regions resolved: eastus2 (was: eastus)` → fallback happened
- `region X excluded` → SKU map said false or quota exhausted

If user runs into "no regions available", instruct them to run
`refresh-quota` (quota may have changed) or pick a different SKU.

If both `region-quota.yaml` AND `~/.ddos/sku-availability.json` exist, the
runner prefers `region-quota.yaml` because it also carries family quota
counters (the SKU cache only knows availability, not quota).

---

## 8. Cleanup verification

After **every** `ddos run`, regardless of exit reason, the tool fires
`az group delete --no-wait` against `ddos-<runName>-<suffix>`. The
runner's `defer` uses `context.Background()` so this fires even on
Ctrl+C / panic.

To verify deletion from CLI:
```powershell
az group show -n ddos-<runName>-<suffix>    # should return "ResourceGroupNotFound" after a few min
```

If something looked stuck, the safety-net Function in `ddos-safety-net`
RG will pick it up within 5 min (matches `tags["auto-delete-at"] < now`).

---

## 9. Common failure modes & responses

| Symptom (in run.log) | Cause | Action |
|---|---|---|
| `QuotaExceeded ... LowPriorityCores ... Additional Required: 384` | Manual `initialVMCount` set as total instead of per-region (or quota actually exhausted) | Omit `initialVMCount` to let the runner auto-derive (per-region), or divide your value by `len(regions)` |
| `region X excluded: SKU not available` | SKU restricted in that region | `refresh-quota`; or change `vmSize`; or drop that region from `regions:` list |
| `observed 0 RPS / target N` for >2 ticks early | VMs deployed but K6 not started OR `runid` tag mismatch | Check Grafana → "No data" usually means workers failed to fetch bundle. SSH only via Bastion (no public 22). |
| `⚠ all VMSS at per-VMSS hard cap (200)` | Fleet too small + can't add fresh regions | Increase `regions:` in profile, or bump `maxVMCount` and accept multi-VMSS-per-region |
| K6 saturating at low RPS per VM | Likely **target endpoint** can't handle keepalive — `keepalive-burst` reuses ~50 connections per VU; if target rate-limits per conn it'll throttle us | Verify with `curl -v` against target. Not a tool bug. |
| Spot eviction storm | Spot pool tight in that region | Tool auto-refills. If happening 3+ times per VMSS, retry test or pick another region (Azure spot pricing/availability fluctuates) |

---

## 10. Commands reference (full list)

| Command | Frequency | Purpose |
|---|---|---|
| `ddos init-observability` | One-time per sub | Deploy AMW + DCE + DCR + Managed Grafana |
| `ddos init-safety-net` | One-time per sub | Deploy cleanup Function App (FC1 Flex Consumption) |
| `ddos refresh-quota` | When quota changes | Refresh `region-quota.yaml` |
| `ddos refresh-sku-cache` | Optional | Refresh `~/.ddos/sku-availability.json` (fallback only) |
| `ddos validate <file>` | Pre-flight | Schema-check and estimate maximum cost; supports `--json` and `--max-cost` (no Azure calls) |
| `ddos run <file>` | **The main command** | Full lifecycle: deploy → load → cleanup |
| `ddos destroy <run-id-or-rg>` | Recovery | `az group delete --no-wait` (refuses `ddos-observability`) |
| `ddos observe` | Anytime | Open Grafana in browser (`--run-id` pre-filters) |
| `ddos doctor` | Troubleshooting | Verify az login, observability stack, tools |

Build: `go build -o ddos.exe ./cmd/ddos` (Go 1.26+, with patched Go 1.26.6 selected by `go.mod`).

---

## 11. Quick recipe by user intent

### "Run a 5k test"
```powershell
.\ddos.exe validate smoke.local.yaml --json --max-cost <approved-usd>
.\ddos.exe run smoke.local.yaml --max-cost <approved-usd>
```
Expected: ~5 000 RPS sustained, 1 region, 1 VM (auto-derived), ~6 min total wall-clock.

### "Run a 200k test"
```powershell
.\ddos.exe validate attack-200k.local.yaml --json --max-cost <approved-usd>
.\ddos.exe run attack-200k.local.yaml --max-cost <approved-usd>
```
Expected initial fleet: 40 VMs total (10/region across 4 regions), with up to
84 total under the profile's scale-out ceiling.

### "Run a 1M test"
**Stop and confirm with user first**:
- Authorized target endpoint URLs (all defaults are placeholders)
- A budget above the profile-aware estimate from `validate --json`
- Aggregate quota for **200 F4s_v2 VMs** (= 800 Fsv2-family vCPUs). The empty
  `regions` list auto-expands through `region-quota.yaml`, but `maxVMCount: 200`
  leaves no scale-out headroom.
- Then run the edited local profile with `--max-cost <approved-usd>`.

### "What's the status of my last run?"
```powershell
Get-Content runs\<runName>-<suffix>\run.log -Tail 30
```

### "Did the cleanup work?"
```powershell
az group list --query "[?starts_with(name,'ddos-') && tags.purpose=='ddos-loadgen'].name" -o tsv
```

---

## 12. What NOT to do

- ❌ Don't manually set `initialVMCount` above the per-region quota — see §3
  sizing. Omitting it lets the runner derive a safe per-region value automatically.
- ❌ Don't run > 1 test concurrently against the same target (metrics will
  collide unless `runName` differs, and target may already be saturated).
- ❌ Don't manually delete the per-run RG mid-flight — the runner's cleanup
  will race you. Instead Ctrl+C the runner (graceful cleanup fires).
- ❌ Don't add new K6 scenarios without thinking — patterns `heavy-payload`,
  `light-payload`, `mixed` were intentionally removed. Only
  `keepalive-burst` and `new-conn-flood` exist now.
- ❌ Don't enable K6 summary output (`--summary-export`, removing
  `--no-summary`, or re-adding `summaryTrendStats`). Trend sinks balloon
  to gigabytes at high RPS.

---

## 13. Source-of-truth files (when in doubt, read these)

- **Lifecycle**: `internal/orchestrator/runner/runner.go` (1 file, ~700 lines)
- **Scaling**: same file, `scaleFleet()` function
- **Profile schema**: `internal/config/schema.go`
  - `endpoints[].resolveToIP` (optional): pin TCP connect destination to a specific IP.
    Host header and TLS SNI remain the original hostname. Validated as a valid IP at config
    parse time. Propagated to `profile.json` as `resolve_to_ip` and injected into K6
    `options.hosts` at worker init.
- **K6 entrypoint**: `k6/runner.js`
- **Worker bootstrap**: `cloud-init/worker.yaml.tmpl` +
  `cloud-init/units/ddos-worker.service`
- **Quota/SKU cache**: `internal/azure/quotacache/quotacache.go` +
  `region-quota.yaml`
- **Architecture deep-dive**: [FLOW.md](./FLOW.md) (component-by-component walk)
- **Cost estimates**: [COST.md](./COST.md) (50k–1M RPS × 1-hour pricing table)
