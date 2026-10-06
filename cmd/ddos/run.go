package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/patenvy1234/Http_K6_DDOS/internal/config/userconfig"
	"github.com/patenvy1234/Http_K6_DDOS/internal/orchestrator/runner"
)

func newRunCmd() *cobra.Command {
	var maxCost float64
	cmd := &cobra.Command{
		Use:   "run <attack.yaml>",
		Short: "Deploy the fleet, drive traffic, auto-scale, and clean up.",
		Long: `Executes a full load run:
  1. Validates the attack profile.
  2. Creates a per-run resource group.
  3. Deploys UAMI + storage (foundation phase).
  4. Renders cloud-init with the known storage account name.
  5. Builds bundle.tar.gz (k6 scripts + profile.json) and uploads to blob.
  6. Deploys VMSS fleet across configured regions (fleet phase).
  7. Monitors for spot evictions and scales to maintain target VM count.
  8. Deletes the resource group on completion or Ctrl-C.

Prerequisites: run 'ddos init-observability' once to create the persistent
monitoring stack, then set your subscription / observability config.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			uc, err := userconfig.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if uc.Observability.DCEURL == "" {
				return fmt.Errorf("observability config missing — run `ddos init-observability` first")
			}
			return runner.Run(context.Background(), args[0], uc, runner.Options{
				MaxCostUSD: maxCost,
			})
		},
	}
	cmd.Flags().Float64Var(&maxCost, "max-cost", 200, "Refuse to deploy if estimated run cost (USD) exceeds this ceiling; 0 disables the guard")
	return cmd
}
