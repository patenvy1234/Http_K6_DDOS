package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/azcli"
)

func newDestroyCmd() *cobra.Command {
	var rgName string
	cmd := &cobra.Command{
		Use:   "destroy <run-id-or-rg>",
		Short: "Force-delete a per-run resource group.",
		Long: `Force-deletes a resource group created by `+"`ddos run`"+`. Use only when the
in-process cleanup somehow failed (e.g., killed the controller with SIGKILL).
The subscription-level deadman function will pick up the slack within ~5 min
either way, but this is faster.

Refuses to delete the persistent ddos-observability RG.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && rgName == "" {
				rgName = args[0]
			}
			if rgName == "" {
				return fmt.Errorf("destroy: provide a resource group name or --rg flag")
			}
			if rgName == "ddos-observability" {
				return fmt.Errorf("refusing to delete the persistent observability RG; use `az group delete` if you really mean it")
			}
			az := azcli.New()
			return destroyRG(cmd.Context(), az, rgName)
		},
	}
	cmd.Flags().StringVar(&rgName, "rg", "", "Resource group name (alternative to positional arg)")
	return cmd
}

func destroyRG(ctx context.Context, az *azcli.Runner, rg string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	fmt.Printf("→ Deleting resource group %s (no-wait) ...\n", rg)
	if _, err := az.Run(ctx, "group", "delete", "--name", rg, "--yes", "--no-wait"); err != nil {
		return err
	}
	fmt.Println("✓ Delete initiated (Azure will continue in the background).")
	return nil
}
