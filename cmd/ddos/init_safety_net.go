package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	ddospkg "github.com/patenvy1234/Http_K6_DDOS"
	"github.com/spf13/cobra"

	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/azcli"
	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/bicepdeploy"
)

const safetyNetRG = "ddos-safety-net"

func newInitSafetyNetCmd() *cobra.Command {
	var (
		subscription string
		location     string
	)
	cmd := &cobra.Command{
		Use:   "init-safety-net",
		Short: "Deploy the subscription-level cleanup deadman Azure Function (one-time).",
		Long: `init-safety-net deploys a timer-triggered Azure Function that runs every
5 minutes, scans for resource groups tagged purpose=ddos-loadgen whose
auto-delete-at timestamp is in the past, and deletes them.

This is the last line of defence against orphaned spend — it fires even if
the CLI crashes or the operator loses network connectivity mid-run.

Skips the persistent ddos-observability RG and any RG without the
purpose=ddos-loadgen tag.

Deploy once per subscription; survives across tool versions.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInitSafetyNet(cmd.Context(), subscription, location)
		},
	}
	cmd.Flags().StringVar(&subscription, "subscription", "", "Azure subscription ID (defaults to current az context)")
	cmd.Flags().StringVar(&location, "location", "eastus", "Azure region for the safety-net Function App")
	return cmd
}

func runInitSafetyNet(ctx context.Context, subscription, location string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	az := azcli.New()

	// 1) Resolve subscription.
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
	}
	fmt.Printf("→ Subscription: %s (%s)\n", acct.Name, subscription)

	// 2) Ensure safety-net RG exists.
	fmt.Printf("→ Creating / verifying RG %s in %s ...\n", safetyNetRG, location)
	if err := az.CreateResourceGroup(ctx, safetyNetRG, location); err != nil {
		return fmt.Errorf("create safety-net RG: %w", err)
	}

	// 3) Deploy Bicep (Consumption Function App + storage + system MI + Contributor role).
	tpl, err := locateTemplate("infra/bicep/cleanup-function.bicep")
	if err != nil {
		return err
	}
	fmt.Printf("→ Deploying cleanup function infra (%s) ...\n", tpl)
	outputs, err := bicepdeploy.ResourceGroupScope(ctx, az, safetyNetRG, bicepdeploy.Options{
		Name:         "ddos-safety-net-deploy",
		Location:     location,
		TemplateFile: tpl,
		Parameters: map[string]any{
			"location":       location,
			"subscriptionId": subscription,
		},
	})
	if err != nil {
		return fmt.Errorf("bicep deploy cleanup-function: %w", err)
	}
	fnAppName := outputs.String("functionAppName")
	if fnAppName == "" {
		return fmt.Errorf("Bicep did not return functionAppName output")
	}
	fmt.Printf("→ Function App: %s\n", fnAppName)

	// 4) Build function zip from embedded infra/function files.
	fmt.Println("→ Building function deployment package ...")
	zipBytes, err := buildFunctionZip()
	if err != nil {
		return fmt.Errorf("build function zip: %w", err)
	}

	// 5) Write zip to temp file and deploy.
	tmp, err := os.CreateTemp("", "ddos-fn-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(zipBytes); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	fmt.Printf("→ Deploying function code (%d KB) ...\n", len(zipBytes)/1024)
	if _, err := az.Run(ctx,
		"functionapp", "deployment", "source", "config-zip",
		"--resource-group", safetyNetRG,
		"--name", fnAppName,
		"--src", tmp.Name(),
	); err != nil {
		return fmt.Errorf("deploy function code: %w", err)
	}

	fmt.Println()
	fmt.Println("✓ Safety-net function deployed.")
	fmt.Printf("  Function App:  %s\n", fnAppName)
	fmt.Printf("  RG:            %s\n", safetyNetRG)
	fmt.Println("  Schedule:      every 5 minutes (0 */5 * * * *)")
	fmt.Println()
	fmt.Println("Next step:")
	fmt.Println("  ddos validate examples/attack-simple.yaml")
	fmt.Println("  ddos run examples/attack-simple.yaml")
	return nil
}

// buildFunctionZip walks infra/function from the embedded FS and produces an
// in-memory zip suitable for `az functionapp deployment source config-zip`.
func buildFunctionZip() ([]byte, error) {
	subFS, err := fs.Sub(ddospkg.InfraFS, "infra/function")
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	err = fs.WalkDir(subFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(subFS, path)
		if err != nil {
			return err
		}
		// Normalise path separators for zip (always forward slashes).
		zipPath := filepath.ToSlash(path)
		w, err := zw.Create(zipPath)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
