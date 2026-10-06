# DDOS — End-to-End Flow Reference

This document is a **component-by-component, phase-by-phase trace** of what every
file in this repo does, and at which point in the lifecycle of the tool it runs.

The tool has **three lifecycle scopes**:

| Scope | When it runs | Who triggers it | Lives in |
|---|---|---|---|
| **Build-time** | `go build ./cmd/ddos` | Developer | Local machine |
| **Operator-time** | `ddos <subcommand>` | CLI user (you) | Local machine + Azure control plane |
| **Worker-time** | Inside each spot VM | cloud-init → systemd | Azure VMSS instance |

> **Operator-time** is further split into **one-time setup** (`init-observability`,
> `init-safety-net`, `refresh-quota`) and **per-run** (`validate`, `run`, `destroy`,
> `observe`, `doctor`).

---

## Table of contents

1. [Repo layout](#repo-layout)
2. [Build-time](#build-time)
3. [Operator one-time setup](#operator-one-time-setup)
4. [Operator per-run (`ddos run`) — lifecycle](#operator-per-run-ddos-run--lifecycle)
5. [Worker-time inside the VM](#worker-time-inside-the-vm)
6. [Observability data flow](#observability-data-flow)
7. [Cleanup paths (three lines of defence)](#cleanup-paths-three-lines-of-defence)
8. [File index — what each file does and when](#file-index--what-each-file-does-and-when)
9. [End-to-end walkthrough](#end-to-end-walkthrough)
10. [Recent design choices](#recent-design-choices)

---

## Repo layout

```
DDOS/
├── ddos.exe                         # Built CLI binary (Windows; gitignored)
├── go.mod / go.sum                  # Go module definition
├── assets.go                        # //go:embed cloud-init       → CloudInitFS
├── k6assets.go                      # //go:embed k6               → K6FS
├── infrassets.go                    # //go:embed infra/grafana
│                                    #          infra/function     → InfraFS
│
├── cmd/ddos/                        # CLI entry points (cobra commands)
│   ├── main.go                      # Wires up subcommands
│   ├── init_observability.go        # One-time persistent obs stack deploy
│   ├── init_safety_net.go           # One-time cleanup-deadman function deploy
│   ├── refresh_quota.go             # Refresh region-quota.yaml (per sub)
│   ├── refresh_sku_cache.go         # Refresh ~/.ddos/sku-availability.json
│   ├── validate.go                  # Lints + parses an attack.yaml
│   ├── run.go                       # The orchestrator entry point (per-run)
│   ├── destroy.go                   # Manual RG teardown
│   ├── observe.go                   # Opens Grafana in the browser
│   └── doctor.go                    # Environment health check
│
├── internal/
│   ├── config/
│   │   ├── schema.go                # AttackProfile struct + validation
│   │   ├── load.go                  # YAML loader
│   │   ├── runname.go               # runName regex constraints
│   │   └── userconfig/userconfig.go # ~/.ddos/config.yaml persistence
│   ├── azure/
│   │   ├── azcli/azcli.go           # az CLI subprocess wrapper
│   │   ├── bicepdeploy/bicepdeploy.go  # Subscription/RG-scoped deploy + outputs
│   │   ├── quotacache/quotacache.go    # region-quota.yaml reader + planner
│   │   └── skucache/skucache.go        # ~/.ddos/sku-availability.json reader
│   ├── orchestrator/
│   │   ├── runspec/runspec.go       # Per-run identity: runID, SSH key, tags
│   │   ├── cloudinit/cloudinit.go   # Renders worker.yaml.tmpl
│   │   ├── bundle/bundle.go         # Builds the k6 tarball uploaded to blob
│   │   ├── profilegen/profilegen.go # Builds profile.json for k6
│   │   └── runner/runner.go         # The lifecycle controller (watch loop, scaler)
│   ├── grafana/grafana.go           # az grafana dashboard import wrapper
│   └── monitor/amw.go               # AMW Prometheus query client (auto-scaler)
│
├── infra/
│   ├── bicep/
│   │   ├── observability.bicep      # ONE-TIME: AMW + DCE + DCR + Grafana
│   │   ├── cleanup-function.bicep   # ONE-TIME: Flex-Consumption Python timer
│   │   ├── foundation.bicep         # PER-RUN phase-1: UAMI + storage + roles
│   │   ├── fleet.bicep              # PER-RUN phase-2: per-region VMSS (initial)
│   │   ├── fleet-add-region.bicep   # AT RUNTIME: scaler adds a fresh region
│   │   └── modules/
│   │       ├── observability_inner.bicep  # RG-scoped child of observability
│   │       ├── region.bicep         # VNet + NSG + per-VM Public IPs + VMSS
│   │       ├── role_dcr.bicep       # Cross-RG: Worker UAMI → DCR publisher
│   │       └── role_subscription.bicep    # Subscription-scope role assignment
│   ├── grafana/
│   │   ├── overview.json            # Global RPS / error / latency dashboard
│   │   └── per-endpoint.json        # Per-endpoint slice dashboard
│   └── function/                    # Safety-net Python Azure Function
│       ├── function_app.py          # Timer trigger — deletes expired RGs
│       ├── host.json                # Functions runtime config
│       └── requirements.txt         # azure-identity, azure-mgmt-resource
│
├── cloud-init/                      # Runs INSIDE each spot VM at first boot
│   ├── worker.yaml.tmpl             # Go-templated cloud-init root document
│   ├── sysctl/99-ddos.conf          # Kernel tunables for high RPS
│   ├── limits/99-ddos.conf          # nofile = 1 048 576
│   ├── units/
│   │   ├── ddos-worker.service           # k6 systemd unit
│   │   └── ddos-token-refresh.service    # Entra token refresh sidecar
│   └── scripts/
│       ├── ddos-imds-token.sh       # Fetches IMDS access tokens
│       ├── ddos-token-refresh.sh    # Loops to keep token fresh in /etc/ddos/k6.env
│       ├── ddos-install-k6.sh       # Installs k6 binary from GitHub release
│       ├── ddos-fetch-bundle.sh     # Downloads k6 tarball from blob (UAMI auth)
│       ├── ddos-bootstrap-tags.sh   # Reads IMDS metadata into /etc/ddos/tags.env
│       └── ddos-wait-for-start.sh   # Sleeps until profile.json.start_at
│
├── k6/                              # k6 JS code, packed as bundle.tar.gz at run-time
│   ├── runner.js                    # Single entry point (other scenarios kept for ref)
│   ├── lib/
│   │   ├── profile.js               # Reads /etc/ddos/profile.json + helpers
│   │   ├── transport.js             # http.Client params, payload builder
│   │   └── metrics.js               # No-op shim; native k6 metrics feed the scaler
│   └── scenarios/
│       ├── keepalive-burst.js       # Self-contained alt entry (not used by default)
│       └── new-conn-flood.js        # Self-contained alt entry (not used by default)
│
├── region-quota.yaml                # Per-sub quota & SKU availability snapshot
├── runs/                            # Per-test transcripts (gitignored)
├── docs/                            # All docs (README, AGENTS, FLOW, COST)
└── examples/                        # Sample attack.yaml configurations
    ├── attack-simple.yaml           # Single endpoint, ~5 000 RPS
    ├── attack-200k.yaml             # 200k RPS across 4 regions × 10 F4s_v2 (auto-derived)
    └── attack-1m.yaml               # 1M-RPS preset (replace target URL before use)
```

> **Note on `k6/scenarios/`**: `runner.js` is the actual entry point invoked by
> systemd; it dispatches via `profile.pattern` and uses `lib/transport.js` to
> apply global connection-reuse settings. The standalone files in
> `scenarios/` are kept as reference implementations of each pattern — they are
> shipped in the bundle but no current code path executes them directly.

---

## Build-time

When you run `go build -o ddos.exe ./cmd/ddos` the Go compiler **embeds** three
filesystems into the binary so the CLI ships standalone:

| Embed file | What it embeds | Used by |
|---|---|---|
| `assets.go` | `cloud-init/` directory | `internal/orchestrator/cloudinit` (template + scripts + units + sysctl/limits) |
| `k6assets.go` | `k6/` directory | `internal/orchestrator/bundle` (k6 tarball) |
| `infrassets.go` | `infra/grafana/`, `infra/function/` | `internal/grafana` (dashboard import), `cmd/ddos/init_safety_net.go` (function zip) |

Bicep files under `infra/bicep/` are **not** embedded — they are read from disk
at runtime via `cmd/ddos/init_observability.go:locateTemplate`, which searches:

1. `./<rel>` (current dir)
2. `<exe-dir>/<rel>` (next to the binary)
3. `<exe-dir>/../<rel>` (parent of the binary)

So you can either run `ddos` from the repo root or place the binary next to a
copy of the `infra/` tree.

Go module:

- Module path: `github.com/patenvy1234/Http_K6_DDOS`
- `go.mod` requires `go 1.25.0` (current toolchain may be newer)
- Direct deps: `cobra`, `yaml.v3`, `x/crypto` (RSA SSH key gen), `google/uuid`

---

## Operator one-time setup

Three commands provision persistent infrastructure that survives across runs.
Run them once per Azure subscription (in any order, though the order below is
recommended):

### `ddos init-observability`

Deploys the persistent monitoring stack.

```
cmd/ddos/init_observability.go
  ├─► az ad signed-in-user show           (get operator object ID)
  ├─► locateTemplate("infra/bicep/observability.bicep")
  ├─► bicepdeploy.SubscriptionScope(...)
  │     │
  │     ▼
  │   az deployment sub create
  │     └─► infra/bicep/observability.bicep
  │           ├─► resource rg          (creates ddos-observability RG)
  │           └─► module obs           (scope: rg)
  │                 │ infra/bicep/modules/observability_inner.bicep
  │                 ├── Microsoft.Monitor/accounts                  (AMW)
  │                 ├── Microsoft.Insights/dataCollectionEndpoints  (DCE)
  │                 ├── Microsoft.Insights/dataCollectionRules      (DCR)
  │                 ├── Microsoft.Dashboard/grafana                 (Standard SKU)
  │                 └── role assignments
  │                       • Grafana MI → Monitoring Data Reader on AMW
  │                       • Operator  → Grafana Admin
  │
  ├─► outputs persisted to ~/.ddos/config.yaml via
  │     internal/config/userconfig/userconfig.go
  │       • workspaceResourceId, workspaceName
  │       • dceUrl, dceResourceId
  │       • dcrResourceId, dcrImmutableId
  │       • grafanaUrl, grafanaResourceId
  │       • prometheusQueryUrl
  │
  └─► grafana.DeployDashboards(...)
        └─► for each .json in InfraFS["infra/grafana"]:
              az grafana dashboard import --definition <tmpfile>
                • infra/grafana/overview.json
                • infra/grafana/per-endpoint.json
```

**Result:** A long-lived `ddos-observability` RG containing AMW, DCE, DCR, and
Managed Grafana, with two dashboards pre-loaded. Every subsequent run pushes
metrics into the same AMW.

### `ddos init-safety-net`

Deploys the last-line-of-defence cleanup function.

```
cmd/ddos/init_safety_net.go
  ├─► az.CreateResourceGroup("ddos-safety-net", location)
  ├─► locateTemplate("infra/bicep/cleanup-function.bicep")
  ├─► bicepdeploy.ResourceGroupScope(...)
  │     │
  │     ▼
  │   az deployment group create
  │     └─► infra/bicep/cleanup-function.bicep
  │           ├── Microsoft.Storage/storageAccounts   (shared-key disabled)
  │           ├── Microsoft.Insights/components       (App Insights)
  │           ├── Microsoft.Web/serverfarms           (FC1 Flex Consumption)
  │           ├── Microsoft.Web/sites                 (Python 3.11 fn app + SAMI)
  │           ├── Storage Blob Data Owner role        (fn MI → its own storage)
  │           └── module contributorRole
  │                 │ infra/bicep/modules/role_subscription.bicep
  │                 └── fn MI → Contributor at /subscriptions/<id>
  │
  ├─► buildFunctionZip()                 (in-memory archive/zip from InfraFS)
  │     • function_app.py
  │     • host.json
  │     • requirements.txt
  │
  └─► az functionapp deployment source config-zip --src <tmpfile>.zip
        (Triggers Oryx remote build → installs azure-identity, azure-mgmt-resource)
```

**Result:** Every 5 minutes the function lists all RGs in the subscription,
filters to `tags.purpose == "ddos-loadgen"` with `tags["auto-delete-at"]` in the
past, and deletes them. The persistent `ddos-observability` and
`ddos-safety-net` RGs are skipped (different tag, plus a name check).

### `ddos refresh-quota`

Generates `region-quota.yaml`, the static cache the runner uses for
quota-aware region selection.

```
cmd/ddos/refresh_quota.go
  ├─► az account show
  ├─► parallel per-region: az vm list-usage --location <r> --output json
  ├─► one global:          az vm list-skus --resource-type virtualMachines --all
  │       (the global query avoids per-region rate-limiting)
  └─► quotacache.Save("region-quota.yaml", file)

YAML shape (one region):
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

Re-run after a quota increase, after Microsoft adds/removes a SKU from a
region, or whenever the runner logs `no quota cache`.

> A companion command `ddos refresh-sku-cache` writes a richer
> `~/.ddos/sku-availability.json` (per-user) that the runner falls back to when
> `region-quota.yaml` is missing. You normally only need `refresh-quota`.

---

## Operator per-run (`ddos run`) — lifecycle

This is the heart of the tool. All steps live in
`internal/orchestrator/runner/runner.go::Run`, executed sequentially with a
single top-level `defer` block that guarantees teardown even on Ctrl+C
(signal-aware via `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`).

```
                       ddos run smoke.local.yaml --max-cost 10
                                       │
                                       ▼
                              cmd/ddos/run.go
                                       │
                                       │ (loads ~/.ddos/config.yaml)
                                       ▼
                       internal/orchestrator/runner.Run(ctx, cfg)
                                       │
   ┌───────────────────────────────────┼───────────────────────────────────────┐
   │
   │   Step 1 — Validate profile
   │   internal/config/load.go + schema.go
   │     • Parses attack.yaml
   │     • Schema checks: VM SKU allowlist (F4s_v2, F8s_v2),
   │                      runName regex, RPS ≤ 2 000 000,
   │                      maxVMCount ≤ 500, endpoints[].url valid,
   │                      endpoints[].resolveToIP (if set) is a valid IP,
   │                      maxVMCount × vCPU × 15k ≥ targetRPS
   │
   │   Step 2 — Derive RunSpec
   │   internal/orchestrator/runspec/runspec.go
   │     • RunSuffix = 8-char lowercase hex (from a fresh UUID)
   │     • ResourceGroup = "ddos-<runName>-<suffix>"
   │     • Fresh RSA-3072 SSH keypair (only public key sent to VM)
   │     • StartAt = now + 5 min (synchronized fleet-wide start)
   │     • AutoDeleteAt = now + TTLMinutes (set as RG tag for the deadman)
   │
   │   Step 3 — Create resource group + install signal handler
   │     • az group create --name <rg> --location <regions[0]>
   │     • `defer az.DeleteResourceGroup(context.Background(), rg)` is now
   │       armed; it fires on any exit path (success, error, Ctrl+C).
   │       context.Background() is critical: it survives the cancelled sigCtx.
   │
   │   Step 4 — Deploy foundation (Bicep, RG-scoped)
   │     • bicepdeploy.ResourceGroupScope("ddos-foundation-<suffix>",
   │         infra/bicep/foundation.bicep, params)
   │         ├── Microsoft.ManagedIdentity/userAssignedIdentities (UAMI)
   │         ├── Microsoft.Storage/storageAccounts                (shared-key OFF)
   │         ├── Microsoft.Storage/.../containers ("ddos-run")
   │         ├── role: UAMI → Storage Blob Data Reader (so workers can read)
   │         ├── role: operator → Storage Blob Data Contributor
   │         │       (so the CLI can upload via --auth-mode login)
   │         └── module role_dcr → UAMI → Monitoring Metrics Publisher on DCR
   │     • Outputs: storageAccountName, blobContainerName,
   │                uamiResourceId, uamiClientId, uamiPrincipalId
   │
   │   Step 5 — Render cloud-init (now storage account name is known)
   │   internal/orchestrator/cloudinit/cloudinit.go
   │     • Templates cloud-init/worker.yaml.tmpl with:
   │         RunID, RunName, AdminUsername, SSHPublicKey,
   │         DCEURL, DCRImmutableID, DCRStreamName,
   │         StorageAccount, BlobContainer, BundleBlob, K6Version
   │     • Embeds every helper (scripts/*.sh, units/*.service, sysctl,
   │       limits) inline via the {{ asset "<path>" | indent N }} func
   │     • Computes K6_PROMETHEUS_RW_SERVER_URL from DCEURL + DCRImmutableID
   │     • Output: base64-encoded customData (≤64 KB ARM cap)
   │
   │   Step 6 — Generate profile.json + bundle.tar.gz
   │   internal/orchestrator/profilegen/profilegen.go
  │     • Per-VM RPS is FIXED at profilegen.FixedPerVMRPS = 5,000
   │       (controller scales fleet horizontally to close any deficit).
   │     • Builds profile.json: run_id, run_name, start_at, target_rps=5000,
   │       ramp_*, duration_seconds, pattern, endpoints[]
   │       (each endpoint carries id, url, method, weight, headers,
   │        conn_mode, resolve_to_ip)
   │   internal/orchestrator/bundle/bundle.go
   │     • Walks K6FS, gzip-tars runner.js + lib/*.js + scenarios/*.js
   │     • Appends profile.json into the tarball (pretty-printed)
   │     • Returns bundle bytes (typically ~7 KB)
   │
   │   Step 7 — Upload bundle to blob storage (OAuth/AAD)
   │     • az.BlobUpload(storageAccount, container, "bundle.tar.gz", bytes)
   │       under the hood: az storage blob upload --auth-mode login
   │       Operator was granted Storage Blob Data Contributor in step 4.
   │
   │   Step 7b — Region preflight (quota cache)
   │     • quotacache.Load("region-quota.yaml")  → primary source
   │     • skucache.Load("")                      → fallback (~/.ddos)
   │     • resolveRegions(qcache, cache, vmSize, requested, exclude={})
   │       drops regions where the SKU map says false or quota is exhausted,
   │       padding with the curated defaultCandidateRegions list.
   │
   │   Step 8 — Deploy fleet (Bicep, RG-scoped)
  │     • Initial sizing is checked before creating the resource group.
  │       Failed deployment aborts and initiates cleanup, avoiding duplicate
  │       workers from a partially successful earlier attempt.
  │     • bicepdeploy.ResourceGroupScope("ddos-fleet-<suffix>",
   │         infra/bicep/fleet.bicep, params)
   │         └── for each region (regions array):
   │               module region = infra/bicep/modules/region.bicep
   │                 ├── VNet + Subnet (10.42.1.0/24)
   │                 ├── NSG (deny inbound by default; 5 outbound allows:
   │                 │       AzureMonitor, Storage, AzureActiveDirectory,
   │                 │       Internet:80/443, then deny-all)
   │                 ├── VMSS (Spot, evictionPolicy=Delete, maxPrice=-1)
   │                 │     • Ubuntu 24.04 LTS, ephemeral OS disk (CacheDisk)
   │                 │     • F4s_v2 or F8s_v2
   │                 │     • Per-instance Public IPv4 (Standard SKU) — no NAT GW
   │                 │     • UAMI attached
   │                 │     • customData = the rendered cloud-init
   │                 │     • userData = "UAMI_CLIENT_ID=<client>"
   │     • Outputs: vmssIds[], vmssNames[]
   │
   │   Step 9 — Watch loop (test runtime)
   │     • monitor.New(prometheusQueryURL, az)
   │     • Tick every 30 s until startAt + ramp_up + duration + ramp_down + 3 min.
   │     • Each tick:
   │         (a) refillEvictions: for each VMSS, if live < target capacity,
   │             call az vmss scale to refill (replaces evicted spot VMs).
   │         (b) After warm-up (startAt + ramp_up + 60 s) and a 90 s cooldown:
   │             - Query AMW: sum(rate(k6_http_reqs_total{runid="<sfx>"}[30s]))
   │             - Query AMW: sum(rate(k6_dropped_iterations_total{...}[30s]))
   │             - Saturated  = dropped > 5.0 iter/s
   │             - Below      = observed < 95% × target
   │             - Scale out when saturated OR (below for 2 consecutive ticks).
   │             - Strategy: PREFER a fresh region (quotacache.PlanAllocation
   │               returns a round-robin spread across regions with available
   │               quota that are not yet used and not excluded). Only fall
   │               back to scaling existing VMSS (cap = 200 per VMSS) when no
   │               fresh region is eligible.
   │             - Total fleet cap = profile.Fleet.MaxVMCount; honored strictly.
   │             - On fresh-region SkuNotAvailable, add region to excluded set
   │               and try the next candidate.
   │
   │   Step 10 — Teardown (via the step-3 defer)
   │     • If profile.Lifecycle.cleanupOnInterrupt=true (default):
   │         az.DeleteResourceGroup(context.Background(), spec.ResourceGroup)
   │         which calls: az group delete --name <rg> --yes --no-wait
   │     • runs/<runName>-<suffix>/run.log and profile.yaml persist on disk.
   │
   └───────────────────────────────────────────────────────────────────────────
```

### Other operator commands

| Command | File | What it does |
|---|---|---|
| `ddos validate <file>` | `cmd/ddos/validate.go` | Parses + lints an attack.yaml against `internal/config/schema.go` rules. No Azure calls. Prints the resolved run summary. |
| `ddos destroy <run-id-or-rg>` | `cmd/ddos/destroy.go` | `az group delete --no-wait` on the per-run RG. Refuses to touch `ddos-observability`. |
| `ddos observe` | `cmd/ddos/observe.go` | Opens the Grafana URL from `~/.ddos/config.yaml` in your default browser. `--run-id` appends `var-runId=<id>` as a Grafana template variable. |
| `ddos doctor` | `cmd/ddos/doctor.go` | Health check: `az account show`, `~/.ddos/config.yaml` populated, observability RG exists in Azure, `az bicep version` works. |

---

## Worker-time inside the VM

When a VMSS instance is created in step 8, Azure injects the base64
`customData` (the rendered `worker.yaml.tmpl`) as
`/var/lib/cloud/instance/user-data.txt`. cloud-init then runs through these
stages:

```
            ┌─────────────────────────────────────────────────────────┐
            │  Stage 1 — cloud-init bootstrap (write_files)           │
            │                                                         │
            │  Inline assets written to disk:                         │
            │    /etc/sysctl.d/99-ddos.conf                           │
            │    /etc/security/limits.d/99-ddos.conf                  │
            │    /etc/ddos/runtime.env                                │
            │       • DDOS_RUN_ID, DDOS_RUN_NAME                      │
            │       • DDOS_STORAGE_ACCOUNT, DDOS_BLOB_CONTAINER,      │
            │         DDOS_BUNDLE_BLOB                                │
            │       • DDOS_K6_VERSION                                 │
            │       • K6_PROMETHEUS_RW_SERVER_URL                     │
            │       • K6_OUT=experimental-prometheus-rw               │
            │       • K6_PROMETHEUS_RW_PUSH_INTERVAL=15s              │
            │       • K6_PROMETHEUS_RW_HTTP_REQUEST_TIMEOUT=30s       │
            │       • K6_PROMETHEUS_RW_TREND_STATS="p(95),p(99)"      │
            │                                                         │
            │  Scripts → /usr/local/bin/:                             │
            │    ddos-imds-token, ddos-token-refresh,                 │
            │    ddos-install-k6, ddos-fetch-bundle,                  │
            │    ddos-bootstrap-tags, ddos-wait-for-start             │
            │                                                         │
            │  Systemd units → /etc/systemd/system/:                  │
            │    ddos-token-refresh.service                           │
            │    ddos-worker.service                                  │
            └─────────────────────────────────────────────────────────┘
                                       │
                                       ▼
            ┌─────────────────────────────────────────────────────────┐
            │  Stage 2 — runcmd (sequential)                          │
            │                                                         │
            │  1. mkdir -p /etc/ddos                                  │
            │  2. sysctl --system   (applies 99-ddos.conf:            │
            │     ip_local_port_range, tcp_tw_reuse, somaxconn,       │
            │     tcp_max_syn_backlog, file-max ...)                  │
            │  3. ddos-install-k6                                     │
            │       • Downloads pinned tarball from GitHub releases   │
            │         (https://github.com/grafana/k6/releases/v$VER) │
            │       • Installs /usr/local/bin/k6                      │
            │  4. ddos-bootstrap-tags                                 │
            │       • curl IMDS /metadata/instance                    │
            │       • Writes /etc/ddos/tags.env with:                 │
            │           DDOS_REGION, DDOS_INSTANCE, DDOS_VM_ID        │
            │  5. systemctl daemon-reload                             │
            │  6. systemctl enable --now ddos-token-refresh.service   │
            │  7. systemctl enable --now ddos-worker.service          │
            └─────────────────────────────────────────────────────────┘
                                       │
                                       ▼
            ┌─────────────────────────────────────────────────────────┐
            │  Stage 3 — ddos-token-refresh.service                   │
            │                                                         │
            │  ExecStart=/usr/local/bin/ddos-token-refresh            │
            │  Loop (every 50 minutes):                               │
            │    TOK=$(curl IMDS …?resource=https://monitor.azure.com)│
            │    write atomically to /etc/ddos/k6.env:                │
            │      K6_PROMETHEUS_RW_BEARER_TOKEN=$TOK                 │
            │                                                         │
            │  Token has ~24 h TTL; refresh well before expiry.       │
            │  CPUWeight=20 + MemoryMax=64M cap it so k6 isn't        │
            │  starved.                                               │
            └─────────────────────────────────────────────────────────┘
                                       │
                                       ▼
            ┌─────────────────────────────────────────────────────────┐
            │  Stage 4 — ddos-worker.service                          │
            │                                                         │
            │  EnvironmentFile= /etc/ddos/runtime.env                 │
            │                   /etc/ddos/tags.env                    │
            │                   /etc/ddos/k6.env                      │
            │                                                         │
            │  ExecStartPre=/usr/local/bin/ddos-fetch-bundle          │
            │    • TOK=$(ddos-imds-token https://storage.azure.com)   │
            │    • curl -H "Authorization: Bearer $TOK"               │
            │           <storage-account>.blob.core.windows.net/      │
            │           <container>/<bundle>                          │
            │    • tar -xzf bundle.tar.gz -C /etc/ddos                │
            │    • Result: /etc/ddos/{runner.js, lib/, scenarios/,    │
            │                          profile.json}                  │
            │                                                         │
            │  ExecStartPre=/usr/local/bin/ddos-wait-for-start        │
            │    • Reads profile.json.start_at                        │
            │    • sleep $((start_epoch - now_epoch))                 │
            │    • Synchronized fleet-wide start across all VMs.      │
            │                                                         │
            │  ExecStart= /usr/local/bin/k6 run                       │
            │               --quiet                                   │
            │               --no-summary           (MEMORY CRITICAL)  │
            │               --compatibility-mode=base                 │
            │               --tag runId=$DDOS_RUN_ID                  │
            │               --tag runName=$DDOS_RUN_NAME              │
            │               --tag region=$DDOS_REGION                 │
            │               --tag instance=$DDOS_INSTANCE             │
            │               /etc/ddos/runner.js                       │
            │                                                         │
            │  LimitNOFILE=1048576, LimitNPROC=1048576, GOGC=200      │
            │  KillSignal=SIGINT, TimeoutStopSec=30                   │
            │  Restart=on-failure (no restart loop on TLS/auth errs)  │
            └─────────────────────────────────────────────────────────┘
                                       │
                                       ▼
            ┌─────────────────────────────────────────────────────────┐
            │  k6/runner.js                                           │
            │    • open('/etc/ddos/profile.json') at init time        │
            │    • Builds K6 options.hosts from endpoints[]           │
            │      that carry resolve_to_ip (pins TCP destination     │
            │      while Host header + TLS SNI stay original)         │
            │    • Builds payloads + per-endpoint params              │
            │    • Single scenario: constant-arrival-rate, rate=target│
            │    • Weighted endpoint picker; sendRequest dispatches   │
            │      keepalive vs new-conn params per profile.pattern.  │
            │                                                         │
            │  k6 native metrics flow via Prometheus remote_write:    │
            │    k6_http_reqs_total, k6_http_req_duration{*},         │
            │    k6_iterations_total, k6_dropped_iterations_total ... │
            │    plus the custom ddos_* counters from lib/metrics.js. │
            │                                                         │
            │  Every sample is tagged {runid, runName, region,        │
            │  instance, endpoint}.                                   │
            │                                                         │
            │  POST destination is K6_PROMETHEUS_RW_SERVER_URL:       │
            │    $DCE_URL/dataCollectionRules/                        │
            │      $DCR_IMMUTABLE_ID/streams/                         │
            │      Microsoft-PrometheusMetrics/api/v1/write           │
            │      ?api-version=2023-04-24                            │
            │  with Bearer token from /etc/ddos/k6.env.               │
            └─────────────────────────────────────────────────────────┘
```

---

## Observability data flow

```
   Workers (every 5 s)                          Operator's CLI (every 30 s)
   ──────────────────────                       ─────────────────────────
   k6 → Prom remote_write                       monitor.amw.QueryScalar()
        │                                           │
        ▼                                           │
   DCE (regional ingestion endpoint)                │
        │                                           │
        ▼                                           │
   DCR (Microsoft-PrometheusMetrics stream)         │
        │                                           │
        ▼                                           │
   Azure Monitor Workspace (AMW) ◄──────────────────┤
        │                                           │
        ├──► Grafana datasource                     │
        │       │                                   │  PromQL:
        │       ▼                                   │  sum(rate(
        │     dashboards (overview, per-endpoint)   │    k6_http_reqs_total
        │                                           │    {runid="abcd1234"}[30s]))
        │                                           │
        └──► AMW Prometheus query endpoint ─────────┘
                                                    │
                                                    ▼
                                              Auto-scaler decision
                                              (scale VMSS if < 95% target
                                               or dropped iter/s > 5)
```

---

## Cleanup paths (three lines of defence)

A run's RG (`ddos-<runName>-<suffix>`) can be deleted by **any of three
mechanisms**, in increasing order of robustness:

| # | Trigger | Component | Fires when |
|---|---|---|---|
| 1 | Graceful exit | `runner.Run`'s `defer` in `internal/orchestrator/runner/runner.go` | The CLI finishes normally OR receives SIGINT/SIGTERM. Uses `context.Background()` so it survives the cancelled context. |
| 2 | Manual | `ddos destroy <rg>` (`cmd/ddos/destroy.go`) | Operator runs it after a crash. Refuses to touch `ddos-observability`. |
| 3 | Deadman | `infra/function/function_app.py` timer | Every 5 min; deletes RGs where `tags.purpose == "ddos-loadgen"` AND `tags["auto-delete-at"]` is in the past. |

**The `auto-delete-at` tag is set at RG creation time** (step 2 of the runner,
recorded in step 4 via tags on every resource) to `now + lifecycle.ttlMinutes`.
Even if the CLI disappears the instant the RG is created, the function will
pick it up at most 5 minutes after the deadline elapses.

---

## File index — what each file does and when

The table groups files by lifecycle scope. Items marked **★** are the
load-bearing components — touching them changes core behavior.

### Build-time

| File | Purpose |
|---|---|
| `go.mod`, `go.sum` | Go module declaration. Deps: cobra, yaml.v3, x/crypto (RSA), google/uuid. |
| `assets.go` ★ | `//go:embed cloud-init` → `CloudInitFS`. Used by `internal/orchestrator/cloudinit`. |
| `k6assets.go` ★ | `//go:embed k6` → `K6FS`. Used by `internal/orchestrator/bundle`. |
| `infrassets.go` ★ | `//go:embed infra/grafana infra/function` → `InfraFS`. Used by Grafana dashboard import + safety-net function zip. |

### Operator CLI (`cmd/ddos/*.go`)

| File | When | What it does |
|---|---|---|
| `main.go` | Every invocation | Cobra root; registers all subcommands. |
| `validate.go` | `ddos validate` | Local-only: `config.LoadFile` → `Validate()` → print summary. |
| `init_observability.go` ★ | One-time setup | Deploys `observability.bicep`, persists outputs to `~/.ddos/config.yaml`, imports Grafana dashboards from `InfraFS`. |
| `init_safety_net.go` ★ | One-time setup | Creates `ddos-safety-net` RG → deploys `cleanup-function.bicep` → builds in-memory zip from `InfraFS["infra/function"]` → `az functionapp deployment source config-zip`. |
| `refresh_quota.go` | When quota changes | Parallel `az vm list-usage` + one `az vm list-skus --all` → `region-quota.yaml`. |
| `refresh_sku_cache.go` | Optional | Writes a richer `~/.ddos/sku-availability.json`. Used as fallback by the runner when `region-quota.yaml` is absent. |
| `run.go` ★ | Per-run entry | Loads userconfig → calls `runner.Run(ctx, profile, uc, opts)`. |
| `destroy.go` | Manual | `az group delete --no-wait`. Refuses `ddos-observability`. |
| `observe.go` | Anytime | Opens Grafana URL in default browser (Windows / macOS / Linux). |
| `doctor.go` | Anytime | Sanity-checks az CLI, bicep, logged-in account, `~/.ddos/config.yaml`, observability RG. |

### Internal libraries (`internal/`)

| File | When | What it does |
|---|---|---|
| `config/schema.go` ★ | `validate`, `run` | Defines `AttackProfile` + `Validate()`. Allowlists `Standard_F4s_v2` / `Standard_F8s_v2`, RPS ≤ 2 000 000, maxVMCount ≤ 500. Accepts `fleet.initialVMCount == 0` (= auto-derive). Validates `endpoints[].resolveToIP` is a real IP. Enforces `MaxVMCount × vCPU × 15 000 ≥ targetRPS`. |
| `config/load.go` | `validate`, `run` | `LoadFile(path)` / `LoadAndValidate(path)`. |
| `config/runname.go` | `validate`, `run` | Run-name regex (lowercase alphanumerics + dashes, 3–48 chars). |
| `config/userconfig/userconfig.go` ★ | All commands | Read/write `~/.ddos/config.yaml`. Holds SubscriptionID, TenantID, and Observability sub-struct (WorkspaceResourceID, DCEURL, DCRImmutableID, DCRResourceID, GrafanaURL, GrafanaResourceID, PrometheusQueryURL …). |
| `azure/azcli/azcli.go` ★ | All Azure-touching code | Wraps `os/exec` for `az`. Methods: `Run`, `RunJSON`, `CurrentAccount`, `SetSubscription`, `CreateResourceGroup`, `DeleteResourceGroup`, `BlobUpload` (OAuth via `--auth-mode login`), `GetSignedInUserObjectID`, `VMSSScale`, `VMSSListInstances`. |
| `azure/bicepdeploy/bicepdeploy.go` ★ | `init-*`, `run` | `SubscriptionScope`, `ResourceGroupScope`. Writes parameters to a temp JSON file (`--parameters @<file>`) because inline values blow past Windows' 8 191-char command-line limit once base64 customData is included. Returns a typed `Outputs` map. |
| `azure/quotacache/quotacache.go` ★ | `run`, `refresh-quota` | Loads `region-quota.yaml`. `AvailableVMs(region, sku)` returns the integer count of new VMs that fit. `PlanAllocation(sku, needed, used, excluded)` round-robins needed VMs across fresh regions. |
| `azure/skucache/skucache.go` | `run`, `refresh-sku-cache` | Loads `~/.ddos/sku-availability.json` (fallback only). `AvailableRegions(sku)` returns regions where the SKU is offered without restriction. |
| `orchestrator/runspec/runspec.go` ★ | `run` step 2 | Generates RunSuffix, ResourceGroup name, RSA-3072 SSH keypair, StartAt (= now + 5 min), AutoDeleteAt (= now + ttlMinutes). |
| `orchestrator/cloudinit/cloudinit.go` ★ | `run` step 5 | Templates `cloud-init/worker.yaml.tmpl` with run context + embedded scripts → base64-encoded customData. Validates required inputs early. |
| `orchestrator/bundle/bundle.go` ★ | `run` step 6 | Walks `K6FS` (every `.js` file), normalises CRLF → LF, tar+gzips them plus the supplied profile.json. Returns `[]byte`. |
| `orchestrator/profilegen/profilegen.go` ★ | `run` step 6 | Exports `FixedPerVMRPS = 5_000`. `Generate(profile, runspec)` always sets `target_rps = 5000` regardless of fleet size, serialises `profile.json` (snake_case fields). Rejects mixed-pattern endpoint sets. The runner uses `FixedPerVMRPS` to auto-derive `vmPerRegion = ⌈⌈targetRPS / 5000⌉ / numRegions⌉` when `fleet.initialVMCount` is `0`. |
| `orchestrator/runner/runner.go` ★ | `run` (whole) | The lifecycle controller. Owns the `defer` cleanup, the watch loop (combined eviction-refill + AMW-driven autoscaler), capacity-error retry logic, and per-test log file (`runs/<runName>-<suffix>/run.log`). |
| `grafana/grafana.go` | `init-observability` | `DeployDashboards(ctx, az, name, rg, fs.FS)` → `az grafana dashboard import` per JSON. Non-fatal on individual dashboard errors. |
| `monitor/amw.go` ★ | `run` watch loop | `QueryScalar(promQL)`, `ObservedRPS(runID)`, `ObservedDroppedRate(runID)`. Auth via `az account get-access-token --resource https://prometheus.monitor.azure.com`. |

### Infrastructure (`infra/bicep/*.bicep`)

| File | Scope | When | What it provisions |
|---|---|---|---|
| `observability.bicep` ★ | `subscription` | `init-observability` | Creates `ddos-observability` RG, then delegates to `observability_inner.bicep`. |
| `modules/observability_inner.bicep` ★ | `resourceGroup` | child of above | AMW (`Microsoft.Monitor/accounts`) + DCE + DCR + Grafana (Standard SKU) + role assignments (Grafana MI → Monitoring Data Reader, operator → Grafana Admin). |
| `cleanup-function.bicep` ★ | `resourceGroup` | `init-safety-net` | Storage (shared-key OFF) + App Insights + FC1 Flex Consumption plan + Python 3.11 Function App (system MI) + Storage Blob Data Owner role + `role_subscription` module (Contributor at sub scope). |
| `modules/role_subscription.bicep` | `subscription` | child of above | Idempotent role assignment at subscription scope. |
| `foundation.bicep` ★ | `resourceGroup` | `run` step 4 | UAMI + storage account + blob container + UAMI→reader/operator→contributor on storage + `role_dcr` module for cross-RG DCR publisher role. |
| `modules/role_dcr.bicep` | `resourceGroup` (obs RG) | child of foundation | UAMI → Monitoring Metrics Publisher on the persistent DCR. |
| `fleet.bicep` ★ | `resourceGroup` | `run` step 8 (initial) | Loops over `regions[]`; for each, includes `region.bicep`. Accepts UAMI IDs + pre-rendered `customDataBase64`. |
| `fleet-add-region.bicep` ★ | `resourceGroup` | `run` watch loop (scale-out) | Single-region VMSS deploy via the same `region.bicep` module. Used by `addRegionVMSS` when the scaler picks a fresh region. |
| `modules/region.bicep` ★ | `resourceGroup` | child of fleet | VNet + Subnet + NSG (deny inbound by default; 5 outbound allows) + VMSS (spot, `evictionPolicy=Delete`, ephemeral OS disk, Ubuntu 24.04 LTS, F4s_v2/F8s_v2, UAMI attached, customData injected, per-VM Public IPv4 — no NAT Gateway). |

### Grafana dashboards (`infra/grafana/`)

| File | Audience |
|---|---|
| `overview.json` | Global RPS / error / latency dashboard. Vars: `$datasource`, `$runId`. |
| `per-endpoint.json` | Per-endpoint slice dashboard. Vars: `$datasource`, `$runId`, `$endpoint`. |

### Safety-net function (`infra/function/`)

| File | What |
|---|---|
| `function_app.py` ★ | Python v2-model timer (`@app.timer_trigger("0 */5 * * * *")`). Uses `ManagedIdentityCredential` + `ResourceManagementClient`. Iterates RGs, deletes those where `tags.purpose=="ddos-loadgen"` and `tags["auto-delete-at"] < now`. Skips RGs whose name contains `observability`. |
| `host.json` | Functions runtime config (extension bundle `[4.*, 5.0.0)`). |
| `requirements.txt` | `azure-functions`, `azure-identity`, `azure-mgmt-resource`. |

### Worker bootstrap (`cloud-init/`)

| File | When (inside VM) | What |
|---|---|---|
| `worker.yaml.tmpl` ★ | cloud-init root | Go template; `write_files` for runtime.env + scripts + systemd units + sysctl + limits; `runcmd` for `sysctl --system`, install-k6, bootstrap-tags, then `systemctl enable --now` both units. |
| `sysctl/99-ddos.conf` | applied at boot | `ip_local_port_range = 1024 65535`, `tcp_tw_reuse=1`, `tcp_fin_timeout=15`, `somaxconn=65535`, `tcp_max_syn_backlog=65535`, `file-max=2097152`, `nr_open=1048576`, generous TCP socket buffer ceilings. |
| `limits/99-ddos.conf` | applied at login/systemd | `nofile = 1 048 576` for all users (so k6 can open millions of sockets). |
| `units/ddos-token-refresh.service` | systemd | `ExecStart=/usr/local/bin/ddos-token-refresh`. Restart=always. CPUWeight=20, MemoryMax=64M (so it can't starve k6). |
| `units/ddos-worker.service` | systemd | `ExecStartPre`=fetch-bundle, then wait-for-start. `ExecStart=k6 run --quiet --no-summary --compatibility-mode=base /etc/ddos/runner.js` with EnvironmentFile= runtime.env, tags.env, k6.env. |
| `scripts/ddos-imds-token.sh` | called by others | `curl http://169.254.169.254/metadata/identity/oauth2/token?resource=…` → prints raw JWT. |
| `scripts/ddos-token-refresh.sh` | systemd loop | Every ~50 min: fetch a fresh `https://monitor.azure.com` token and write `K6_PROMETHEUS_RW_BEARER_TOKEN=<TOK>` to `/etc/ddos/k6.env` atomically. |
| `scripts/ddos-install-k6.sh` | runcmd | Downloads pinned k6 tarball from `github.com/grafana/k6/releases/download/v${K6_VERSION}/...` → `/usr/local/bin/k6`. |
| `scripts/ddos-fetch-bundle.sh` | `ExecStartPre` of worker.service | `TOK=$(ddos-imds-token https://storage.azure.com); curl -H "Authorization: Bearer $TOK" <storage>/<container>/bundle.tar.gz | tar -xzf -`. Retries with backoff because the controller may still be uploading. |
| `scripts/ddos-bootstrap-tags.sh` | runcmd | curl IMDS `/metadata/instance` → writes `DDOS_REGION`, `DDOS_INSTANCE`, `DDOS_VM_ID` to `/etc/ddos/tags.env`. |
| `scripts/ddos-wait-for-start.sh` | `ExecStartPre` of worker.service | Reads `start_at` from `/etc/ddos/profile.json`, `sleep $((start_epoch - now_epoch))`. Synchronized fleet-wide start. |

### k6 load-generator (`k6/`)

| File | What |
|---|---|
| `runner.js` ★ | The actual entry point invoked by `ddos-worker.service`. Imports `lib/*.js`, builds a single `constant-arrival-rate` scenario sized by `vuBudget(rps, pattern)`, builds K6 `options.hosts` from endpoints with `resolve_to_ip`, dispatches via `sendRequest`. |
| `lib/profile.js` | `JSON.parse(open('/etc/ddos/profile.json'))`, plus `buildWeightedPicker(endpoints)` and `totalDurationStr(p)`. Validates required fields at init time. |
| `lib/transport.js` | `KEEPALIVE_PARAMS` / `NEW_CONN_PARAMS` (k6 `responseType:'none'`, `throw:false`, `timeout:15s`). `buildPayloads(endpoints, default)` pre-builds POST bodies at init time. `sendRequest(ep, params, body)` dispatches HTTP method. |
| `lib/metrics.js` | Intentional no-op shim. The scaler uses native `k6_http_reqs_total` and `k6_dropped_iterations_total`; avoiding custom series reduces remote-write load and cardinality. |
| `scenarios/keepalive-burst.js` | Self-contained alternate entry script (not used by default; kept for reference). Uses `KEEPALIVE_PARAMS` and sizes its own VU budget. |
| `scenarios/new-conn-flood.js` | Self-contained alternate entry script. Uses `NEW_CONN_PARAMS` (forces `noConnectionReuse: true`). Documents the per-VM-public-IP egress model and sysctl tuning needed for sustained new-conn floods. |

### Examples

| File | Use case |
|---|---|
| `examples/attack-simple.yaml` | One region (`eastus`), one endpoint (`trans.gif`), ~5 000 RPS — smoke test. |
| `examples/attack-200k.yaml` | 200k RPS across 4 regions, fleet auto-derived to 40 × F4s_v2 (10/region), maxVMCount=84. Replace its placeholder target before use. |
| `examples/attack-1m.yaml` | 1M-RPS preset (`regions: []` auto-expands, fleet auto-derived to 200 F4s_v2 VMs, maxVMCount=200). The endpoint URLs default to `example.com` — **replace before use**, approve the estimated cost, and verify aggregate quota. |

---

## End-to-end walkthrough

A canonical full session looks like this:

```
# 1) Once per Azure subscription (operator):
ddos init-observability
   → ddos-observability RG with AMW + DCE + DCR + Grafana + dashboards
ddos init-safety-net
   → ddos-safety-net RG with FC1 Python timer function
ddos refresh-quota
   → region-quota.yaml (region/SKU/vCPU snapshot consumed by the runner)

# 2) Per attack run (operator; copy a template and replace every target first):
ddos validate attack-1m.local.yaml --json --max-cost <approved-usd>
ddos run attack-1m.local.yaml --max-cost <approved-usd>
   ├─ creates ddos-<runName>-<suffix> RG  (tagged auto-delete-at=<deadline>)
   ├─ writes runs/<runName>-<suffix>/{run.log, profile.yaml}
  ├─ deploys foundation + fleet (Bicep, two-phase; aborts on partial failure)
   ├─ uploads bundle.tar.gz via OAuth (`az storage blob upload --auth-mode login`)
   ├─ workers boot → install k6 → fetch bundle (via UAMI Bearer) → wait → ATTACK
   ├─ metrics flow: k6 → DCE → DCR → AMW → Grafana
  ├─ auto-scaler queries AMW every 30 s; scales out when RPS < 95% target for
  │   2 ticks; concurrent dropped iterations increase added headroom
   ├─ growth strategy: fresh region first (round-robin via region-quota.yaml,
   │   then existing VMSS up to per-VMSS cap of 200)
   ├─ Ctrl+C or natural end → `defer` fires → `az group delete --no-wait`
   └─ Grafana dashboards keep the metrics for the AMW retention period

# 3) If the operator's machine crashes mid-run:
   (no action needed)
   → Within ≤5 min of the deadline, the safety-net function deletes the RG.
```

---

## Recent design choices

### Static quota cache + round-robin allocation (`region-quota.yaml`)

`ddos refresh-quota` produces a YAML file at the repo root with one entry per
candidate region. Source: one global `az vm list-skus --all --resource-type
virtualMachines` (~5 MB JSON for the whole sub) plus parallel per-region `az
vm list-usage`. The global query avoids the per-region rate-limiting we hit
when issuing 14 parallel SKU queries.

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

The runner preflight loads this and prunes regions where the SKU is
restricted or family quota is exhausted.
`quotacache.PlanAllocation` then round-robins VMs across eligible regions on
every scale event, preferring fresh regions before doubling up on a used one.

### Per-VM Public IPs (no NAT Gateway)

Workers attach a Standard SKU Public IPv4 directly to each VMSS instance's
primary NIC (`region.bicep` → `publicIPAddressConfiguration`). NAT Gateway was
removed because:

- We have no source-IP allowlist requirement on the target, so the operational
  cost (per-IP $0.004/hr + per-VMSS NAT $0.045/hr + $0.045/GB data fee)
  outweighs the convenience of a fixed egress IP.
- Per-VM IPs give each instance its own ~64 k ephemeral SNAT port budget
  against any single 5-tuple destination, so port exhaustion is not an issue
  at our scale.
- Per-VM IPs also look like distributed sources from the target's POV,
  matching the load profile of real distributed traffic better than a single
  shared NAT IP.

### Per-test logging (`runs/<runName>-<suffix>/`)

`internal/orchestrator/runner/runner.go::openRunLog()` creates this directory
on every `ddos run`:

- `run.log` — every `logf()` line is tee'd here (mutex-protected, Unix
  newlines so cross-platform diffing works).
- `profile.yaml` — a copy of the source attack profile snapshotted via
  `snapshotProfile()` so logs can be correlated with the exact config used.

The runner's `defer closeRunLog()` ensures cleanup output is captured even
after Ctrl+C.

### K6 memory optimization: `--no-summary`

`cloud-init/units/ddos-worker.service` runs:

```
k6 run --quiet --no-summary --compatibility-mode=base \
  --tag runId=$DDOS_RUN_ID --tag runName=$DDOS_RUN_NAME \
  --tag region=$DDOS_REGION --tag instance=$DDOS_INSTANCE \
  /etc/ddos/runner.js
```

- `--quiet` — suppress the progress bar (cosmetic).
- `--no-summary` — **disable end-of-test summary.** Without it, k6 accumulates
  every HTTP sample into trend sinks for `http_req_duration`,
  `http_req_waiting`, etc., per tag combination. At 200k RPS × 7 min that's
  ~84M samples × 7 trend metrics held in memory. Real-time observability
  already streams to Prometheus, so the summary is pure waste.
- `--compatibility-mode=base` — skip Babel transpilation (small startup win;
  Goja's ES6+ is sufficient for our code).

### Per-endpoint `resolveToIP` (optional)

`endpoints[].resolveToIP` in `attack.yaml` (validated as a real IP by
`internal/config/schema.go`) is propagated through `profile.json` as
`resolve_to_ip` and surfaces as K6 `options.hosts[<hostname>] = <ip>` in
`runner.js`.

Effect: the TCP connection is opened to the supplied IP, while the HTTP `Host`
header and TLS SNI both stay set to the original hostname. Useful for testing
a specific origin or VIP directly while bypassing DNS / CDN routing.

> **Edge case**: if two endpoints share the same hostname but specify
> different `resolveToIP` values, the last one written silently wins (K6's
> `options.hosts` map is keyed by hostname). For mixed-IP testing, vary the
> hostname per endpoint (e.g., add a synthetic suffix).
