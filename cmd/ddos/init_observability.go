package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	ddospkg "github.com/patenvy1234/Http_K6_DDOS"
	"github.com/spf13/cobra"

	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/azcli"
	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/bicepdeploy"
	"github.com/patenvy1234/Http_K6_DDOS/internal/config/userconfig"
	"github.com/patenvy1234/Http_K6_DDOS/internal/grafana"
)

func newInitObservabilityCmd() *cobra.Command {
	var (
		subscription string
		location     string
		namePrefix   string
		rgName       string
	)
	cmd := &cobra.Command{
		Use:   "init-observability",
		Short: "Deploy the persistent Azure Monitor + Grafana observability stack (one-time).",
		Long: `init-observability provisions the Azure Monitor Workspace (managed Prometheus),
Data Collection Endpoint, Data Collection Rule, and Azure Managed Grafana (Essential
SKU) used by every subsequent run. Idempotent — re-running on the same RG updates
in place and refreshes ~/.ddos/config.yaml.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInitObservability(cmd.Context(), subscription, location, namePrefix, rgName)
		},
	}

	cmd.Flags().StringVar(&subscription, "subscription", "", "Azure subscription ID (defaults to current az context)")
	cmd.Flags().StringVar(&location, "location", "eastus", "Azure region for the observability RG")
	cmd.Flags().StringVar(&namePrefix, "name-prefix", "ddos", "Short prefix for resource names (3-12 lowercase chars)")
	cmd.Flags().StringVar(&rgName, "resource-group", "ddos-observability", "Name of the persistent observability RG")
	return cmd
}

func runInitObservability(ctx context.Context, subscription, location, namePrefix, rgName string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	az := azcli.New()

	// 1) Resolve subscription + tenant + operator object ID.
	acct, err := az.CurrentAccount(ctx)
	if err != nil {
		return err
	}
	if subscription == "" {
		subscription = acct.ID
	}
	if subscription != acct.ID {
		fmt.Printf("→ switching az context to subscription %s\n", subscription)
		if err := az.SetSubscription(ctx, subscription); err != nil {
			return err
		}
		acct, err = az.CurrentAccount(ctx)
		if err != nil {
			return err
		}
	}
	fmt.Printf("→ Subscription: %s (%s)\n", acct.Name, acct.ID)
	fmt.Printf("→ Tenant:       %s\n", acct.TenantID)
	fmt.Printf("→ Operator:     %s\n", acct.User.Name)

	var oidResp struct {
		ID string `json:"id"`
	}
	if err := az.RunJSON(ctx, &oidResp, "ad", "signed-in-user", "show", "--query", "{id:id}", "--output", "json"); err != nil {
		return fmt.Errorf("fetch operator object ID: %w", err)
	}
	if oidResp.ID == "" {
		return fmt.Errorf("operator object ID is empty (are you signed in with a user, not a service principal?)")
	}

	// 2) Resolve template path. Default to <cwd>/infra/bicep/observability.bicep,
	//    fall back to <exe-dir>/infra/bicep/observability.bicep for installed binaries.
	tpl, err := locateTemplate("infra/bicep/observability.bicep")
	if err != nil {
		return err
	}
	fmt.Printf("→ Template:     %s\n", tpl)

	// 3) Deploy at subscription scope (creates RG, then deploys inner module).
	fmt.Printf("→ Deploying to %s (region %s) — this typically takes 5-10 minutes ...\n", rgName, location)
	outputs, err := bicepdeploy.SubscriptionScope(ctx, az, bicepdeploy.Options{
		Name:         fmt.Sprintf("ddos-obs-%s", location),
		Location:     location,
		TemplateFile: tpl,
		Parameters: map[string]any{
			"location":          location,
			"resourceGroupName": rgName,
			"namePrefix":        namePrefix,
			"operatorObjectId":  oidResp.ID,
		},
	})
	if err != nil {
		return fmt.Errorf("bicep deploy: %w", err)
	}

	// 4) Persist to ~/.ddos/config.yaml.
	cfg, err := userconfig.Load()
	if err != nil {
		return err
	}
	cfg.SubscriptionID = acct.ID
	cfg.TenantID = acct.TenantID
	cfg.Observability = userconfig.Observability{
		ResourceGroup:       rgName,
		Location:            location,
		WorkspaceResourceID: outputs.String("workspaceResourceId"),
		WorkspaceName:       outputs.String("workspaceName"),
		DCEURL:              outputs.String("dceUrl"),
		DCEResourceID:       outputs.String("dceResourceId"),
		DCRImmutableID:      outputs.String("dcrImmutableId"),
		DCRResourceID:       outputs.String("dcrResourceId"),
		GrafanaURL:          outputs.String("grafanaUrl"),
		GrafanaResourceID:   outputs.String("grafanaResourceId"),
		PrometheusQueryURL:  outputs.String("prometheusQueryUrl"),
	}
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save ~/.ddos/config.yaml: %w", err)
	}

	// 5) Deploy Grafana dashboards.
	grafanaName := outputs.String("grafanaName")
	if grafanaName != "" {
		fmt.Println("→ Importing Grafana dashboards ...")
		dashFS, err := fs.Sub(ddospkg.InfraFS, "infra/grafana")
		if err != nil {
			return fmt.Errorf("grafana dashboards sub: %w", err)
		}
		if err := grafana.DeployDashboards(ctx, az, grafanaName, rgName, dashFS); err != nil {
			// Non-fatal: dashboards can be re-imported with a subsequent run.
			fmt.Printf("  ⚠  dashboard import failed (non-fatal): %v\n", err)
			fmt.Println("     Re-run `ddos init-observability` to retry.")
		}
	}

	cfgPath, _ := userconfig.Path()
	fmt.Println()
	fmt.Println("✓ Observability stack ready.")
	fmt.Printf("  Workspace:  %s\n", cfg.Observability.WorkspaceName)
	fmt.Printf("  Grafana:    %s\n", cfg.Observability.GrafanaURL)
	fmt.Printf("  Saved IDs:  %s\n", cfgPath)
	fmt.Println()
	fmt.Println("Next step:")
	fmt.Println("  ddos init-safety-net   # subscription-level cleanup deadman")
	fmt.Println("  ddos validate examples/attack-simple.yaml")
	return nil
}

// locateTemplate searches for a Bicep template at typical install locations.
func locateTemplate(rel string) (string, error) {
	candidates := []string{rel}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, rel))
		candidates = append(candidates, filepath.Join(filepath.Dir(dir), rel))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			abs, _ := filepath.Abs(p)
			return abs, nil
		}
	}
	return "", fmt.Errorf("template %s not found (searched: %v); run from repo root or use --template flag", rel, candidates)
}
