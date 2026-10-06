# Azure k6 Load Testing

A Go CLI for authorized HTTP load tests using Azure VM Scale Sets, k6 workers,
managed identity, and Azure Monitor managed Prometheus. Only test endpoints you
own or have explicit permission to test.

## Build And Check

Go 1.26 or newer is required. The module selects the patched Go 1.26.6 toolchain.
Node.js 24 is used for worker tests; the Azure CLI with Bicep is needed for
infrastructure compilation and deployment.

```powershell
go build ./cmd/ddos
go test -race ./...
go vet ./...
node --experimental-vm-modules --test tests/k6.test.mjs
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

These checks do not deploy Azure resources or send load-test traffic. CI runs
Go checks on Linux and Windows, worker tests with HTTP mocked, shell syntax
checks, Bicep compilation, and vulnerability scanning.

## Run Safely

Follow the [setup guide](docs/README.md) for the persistent monitoring and
cleanup resources. Copy an example into a git-ignored `*.local.yaml`, replace
every placeholder target, and obtain approval for the target, duration, and
USD budget before running it.

```powershell
./ddos.exe doctor
./ddos.exe validate smoke.local.yaml --json --max-cost <approved-usd>
./ddos.exe run smoke.local.yaml --max-cost <approved-usd>
```

On Linux/macOS the built executable is `./ddos`. Local coding agents should
start with [AGENTS.md](AGENTS.md) and the
[authorized activation skill](.github/skills/run-authorized-load-test/SKILL.md).

## Operating Limits

- Initial sizing honors manual counts and never rounds past the total VM
	ceiling. Auto sizing can use fewer eligible regions to fit that ceiling.
- Each worker is configured for 5,000 RPS. Aggregate target RPS is a goal, not
	a global rate limiter; an expanded fleet can exceed it. Actual throughput
	depends on latency, target behavior, VM resources, and metrics delivery.
- Failed initial deployments abort instead of launching a second fleet over
	partially created resources. Uncertain scale operations still consume VM
	budget until cleanup.
- The deadline cancels and joins controller operations before cleanup. Azure
	resource-group deletion is asynchronous and must still be verified.
- The [cost guard](docs/COST.md#cli-budget-guard) is a planning estimate using
	reference prices, not a hard Azure billing cap. It excludes persistent
	observability and target-side charges.
- No live-VM throughput improvement is claimed from local unit tests.

Local profiles, quota snapshots, credentials, and run logs are excluded from
version control. The public repository starts from a reviewed source snapshot
without earlier operational history.

[Setup and usage](docs/README.md) | [Agent operations](docs/AGENTS.md) |
[Architecture](docs/FLOW.md) | [Cost assumptions](docs/COST.md)
