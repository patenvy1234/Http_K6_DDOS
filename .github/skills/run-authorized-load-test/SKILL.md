---
name: run-authorized-load-test
description: "Safely plan and activate an authorized Azure k6 load test. Use when: run load test, activate DDOS profile, execute k6 fleet, start performance test, or validate load-test cost."
---

# Run An Authorized Load Test

Use this workflow only for targets the user owns or is explicitly authorized to
test. Required inputs are a profile path, a positive USD budget, and explicit
target authorization.

1. Read `AGENTS.md` and the selected profile.
2. Reject placeholder targets such as `example.com`. If authorization or budget
   is missing, obtain it before any deployment command.
3. Run `go build -o ddos.exe ./cmd/ddos`.
4. Run `./ddos.exe doctor`. Stop and report any failed prerequisite.
5. Run `./ddos.exe validate <profile> --json --max-cost <budget>`.
6. Parse the JSON and report target URLs, target RPS, duration, VM size, fleet
   ceiling, regions, and `estimatedMaximumCost.totalUsd`.
7. Run `./ddos.exe run <profile> --max-cost <budget>` in the foreground. Do not
   detach it, set an execution timeout, or launch a second run.
8. On completion or interruption, report the run log path and confirm that
   resource-group deletion was initiated. If cleanup failed, surface the exact
   recovery command printed by the CLI.

Never use `--max-cost 0`, change the profile, initialize persistent Azure
resources, or refresh quota unless the user explicitly approved that action.
