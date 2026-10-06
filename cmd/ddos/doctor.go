package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/azcli"
	"github.com/patenvy1234/Http_K6_DDOS/internal/config/userconfig"
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Verify Azure login, observability stack, and tool readiness.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.Context())
		},
	}
}

func runDoctor(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	az := azcli.New()
	checks := []struct {
		name string
		fn   func() error
	}{
		{"az login (account show)", func() error {
			acct, err := az.CurrentAccount(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("    subscription=%s tenant=%s user=%s\n", acct.ID, acct.TenantID, acct.User.Name)
			return nil
		}},
		{"~/.ddos/config.yaml present", func() error {
			cfg, err := userconfig.Load()
			if err != nil {
				return err
			}
			if cfg.Observability.WorkspaceResourceID == "" {
				return fmt.Errorf("observability stack not initialised — run `ddos init-observability`")
			}
			fmt.Printf("    workspace=%s\n", cfg.Observability.WorkspaceName)
			fmt.Printf("    grafana  =%s\n", cfg.Observability.GrafanaURL)
			return nil
		}},
		{"observability RG exists in Azure", func() error {
			cfg, _ := userconfig.Load()
			if cfg == nil || cfg.Observability.ResourceGroup == "" {
				return fmt.Errorf("no observability RG recorded")
			}
			_, err := az.Run(ctx, "group", "show", "--name", cfg.Observability.ResourceGroup, "--output", "none")
			return err
		}},
		{"bicep CLI available", func() error {
			_, err := az.Run(ctx, "bicep", "version")
			return err
		}},
	}

	failed := 0
	for _, c := range checks {
		err := c.fn()
		if err != nil {
			fmt.Printf("  ✗ %s\n      %v\n", c.name, err)
			failed++
			continue
		}
		fmt.Printf("  ✓ %s\n", c.name)
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	fmt.Println("\nAll checks passed.")
	return nil
}
