# Agent Guide

This repository provisions an Azure k6 fleet for authorized load testing. Read
this file before running commands. The deeper operational reference is
[`docs/AGENTS.md`](docs/AGENTS.md).

## Safety Contract

- Only target endpoints the user owns or is explicitly authorized to test.
- Never run a profile containing `example.com` or another placeholder target.
- Before deployment, obtain an explicit target-authorization confirmation and a
  numeric maximum cost in USD from the user.
- Never pass `--max-cost 0` unless the user explicitly asks to disable the guard.
- Do not run multiple tests against the same target concurrently.
- The remote default branch is `main`.

## Build And Verify

From the repository root on Windows:

```powershell
go build -o ddos.exe ./cmd/ddos
go test ./...
az bicep build --file infra/bicep/fleet.bicep --stdout | Out-Null
```

Do not commit `ddos.exe`, `runs/`, local config, or generated credentials.

## Authorized Activation

Use the workspace skill `/run-authorized-load-test`, or follow these steps:

1. Build the CLI and run `./ddos.exe doctor`. Stop and report failed checks.
2. Inspect every URL in the profile. Stop on placeholders or unconfirmed targets.
3. Plan without Azure deployment:

   ```powershell
   ./ddos.exe validate <profile.yaml> --json --max-cost <approved-usd>
   ```

4. Summarize target, RPS, duration, fleet ceiling, and estimated maximum cost.
5. After authorization and budget are explicit, run in the foreground:

   ```powershell
   ./ddos.exe run <profile.yaml> --max-cost <approved-usd>
   ```

6. Let the command finish. Report achieved RPS, scale events, spot evictions,
   cleanup status, and the `runs/<run>-<id>/run.log` path.

If setup is missing, explain the exact `init-observability`, `init-safety-net`,
or `refresh-quota` command needed. Do not create persistent Azure resources
unless the user requested that setup.

## Source Map

- `cmd/ddos/`: Cobra CLI commands.
- `internal/orchestrator/runner/`: deployment, scaling, and cleanup lifecycle.
- `internal/orchestrator/cost/`: conservative pre-deployment budget estimate.
- `cloud-init/`: worker bootstrap, systemd units, and Linux tuning.
- `infra/bicep/`: Azure infrastructure.
- `k6/`: worker request path and metrics output.
- `examples/`: profiles; validate before use and replace placeholder targets.
