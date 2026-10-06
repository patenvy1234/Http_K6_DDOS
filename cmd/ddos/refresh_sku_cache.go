package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/azcli"
	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/skucache"
)

func newRefreshSkuCacheCmd() *cobra.Command {
	var outPath string
	cmd := &cobra.Command{
		Use:   "refresh-sku-cache",
		Short: "Refresh the local VM-SKU availability cache for the current subscription.",
		Long: `Runs a single ` + "`az vm list-skus --resource-type virtualMachines`" + ` for the
current subscription and writes the result to ~/.ddos/sku-availability.json.

This is a slow (1-3 minute) operation, but it only needs to be run once per
subscription or after Microsoft adds/removes a SKU from a region for you.
` + "`ddos run`" + ` consults this cache instead of calling the Azure API on every run.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRefreshSkuCache(cmd.Context(), outPath)
		},
	}
	cmd.Flags().StringVar(&outPath, "out", "", "output file (default ~/.ddos/sku-availability.json)")
	return cmd
}

func runRefreshSkuCache(ctx context.Context, outPath string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	az := azcli.New()

	acct, err := az.CurrentAccount(ctx)
	if err != nil {
		return fmt.Errorf("refresh-sku-cache: %w", err)
	}
	fmt.Printf("subscription: %s (%s)\n", acct.Name, acct.ID)
	fmt.Println("querying az vm list-skus (this takes 1-3 minutes) ...")

	// Subscription-wide enumeration. `--all` returns SKUs even when the
	// subscription has restrictions, so the cache records WHY each region is
	// blocked instead of silently dropping it.
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var entries []skucache.AzSkuEntry
	if err := az.RunJSON(queryCtx, &entries,
		"vm", "list-skus",
		"--resource-type", "virtualMachines",
		"--all",
		"--output", "json",
	); err != nil {
		return fmt.Errorf("refresh-sku-cache: az vm list-skus: %w", err)
	}
	fmt.Printf("got %d SKU entries\n", len(entries))

	cache := skucache.BuildFromAzEntries(acct.ID, acct.Name, entries)

	if outPath == "" {
		p, err := skucache.DefaultPath()
		if err != nil {
			return err
		}
		outPath = p
	}
	if err := skucache.Save(outPath, cache); err != nil {
		return fmt.Errorf("refresh-sku-cache: save: %w", err)
	}

	fmt.Printf("\n✓ cache written: %s\n", outPath)
	fmt.Printf("  subscription : %s\n", cache.SubscriptionName)
	fmt.Printf("  regions seen : %d\n", len(cache.AllRegions))
	fmt.Printf("  total SKUs   : %d\n\n", len(cache.Skus))

	// Spot-check the SKUs the load tool actually deploys.
	fmt.Println("availability summary for ddos-supported SKUs:")
	for _, sku := range []string{"Standard_F4s_v2", "Standard_F8s_v2"} {
		regions := cache.AvailableRegions(sku)
		if len(regions) == 0 {
			fmt.Printf("  %-20s : (not offered)\n", sku)
			continue
		}
		preview := regions
		if len(preview) > 8 {
			preview = preview[:8]
		}
		sort.Strings(preview)
		extra := ""
		if len(regions) > len(preview) {
			extra = fmt.Sprintf(" (+%d more)", len(regions)-len(preview))
		}
		fmt.Printf("  %-20s : %d regions — %v%s\n", sku, len(regions), preview, extra)
	}
	return nil
}
