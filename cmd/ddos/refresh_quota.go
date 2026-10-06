package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/azcli"
	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/quotacache"
)

// candidateRegionsForQuota mirrors the orchestrator's defaultCandidateRegions list.
var candidateRegionsForQuota = []string{
	"eastus", "eastus2", "westus2", "westus3",
	"southcentralus", "centralus", "northcentralus",
	"northeurope", "westeurope", "uksouth", "francecentral",
	"southeastasia", "japaneast", "australiaeast",
}

func newRefreshQuotaCmd() *cobra.Command {
	var outPath string
	cmd := &cobra.Command{
		Use:   "refresh-quota",
		Short: "Refresh the static region-quota.yaml file for the current subscription.",
		Long: `Queries ` + "`az vm list-usage`" + ` for every candidate region in parallel and writes
a static region-quota.yaml to the repo root (or --out path).

This file records the vCPU quota limit and current usage for each Azure region
so that ` + "`ddos run`" + ` can perform quota-aware round-robin VM allocation without
calling the Azure API on every run.

Run this command:
  • Once after provisioning a new subscription
  • After requesting a quota increase in any region
  • Before a large test (e.g. 1M RPS across many regions)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRefreshQuota(cmd.Context(), outPath)
		},
	}
	cmd.Flags().StringVar(&outPath, "out", "region-quota.yaml", "output file path")
	return cmd
}

// azUsageEntry matches one element of `az vm list-usage --output json`.
// NOTE: `limit` and `currentValue` come back as strings in the Azure CLI JSON
// output (not numbers), so we deserialize as json.Number and convert below.
type azUsageEntry struct {
	Name struct {
		Value          string `json:"value"`
		LocalizedValue string `json:"localizedValue"`
	} `json:"name"`
	Limit        json.Number `json:"limit"`
	CurrentValue json.Number `json:"currentValue"`
	Unit         string      `json:"unit"`
}

// azSkuEntry matches one element of `az vm list-skus --output json`.
type azSkuEntry struct {
	Name         string   `json:"name"`
	ResourceType string   `json:"resourceType"`
	Locations    []string `json:"locations"`
	Restrictions []struct {
		Type       string   `json:"type"`
		ReasonCode string   `json:"reasonCode"`
		Values     []string `json:"values"`
	} `json:"restrictions"`
}

type regionResult struct {
	region string
	usage  []azUsageEntry
	err    error
}

func runRefreshQuota(ctx context.Context, outPath string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	az := azcli.New()

	acct, err := az.CurrentAccount(ctx)
	if err != nil {
		return fmt.Errorf("refresh-quota: %w", err)
	}
	fmt.Printf("subscription : %s (%s)\n", acct.Name, acct.ID)
	fmt.Printf("querying %d regions in parallel ...\n", len(candidateRegionsForQuota))

	results := make([]regionResult, len(candidateRegionsForQuota))
	var wg sync.WaitGroup
	for i, region := range candidateRegionsForQuota {
		wg.Add(1)
		go func(idx int, r string) {
			defer wg.Done()
			qCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()

			usageRaw, err := az.Run(qCtx, "vm", "list-usage", "--location", r, "--output", "json")
			if err != nil {
				results[idx] = regionResult{region: r, err: err}
				return
			}
			var usage []azUsageEntry
			if err := json.Unmarshal(usageRaw, &usage); err != nil {
				results[idx] = regionResult{region: r, err: fmt.Errorf("usage parse: %w", err)}
				return
			}
			results[idx] = regionResult{region: r, usage: usage}
		}(i, region)
	}
	wg.Wait()

	// One global SKU availability query — avoids per-region rate limiting.
	// Builds skuAvailByRegion[region][vmSize] = true when SKU is offered and
	// not subscription-restricted.
	fmt.Println("fetching global VM SKU availability ...")
	skuAvailByRegion := map[string]map[string]bool{}
	{
		skuCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		skuRaw, err := az.Run(skuCtx, "vm", "list-skus",
			"--resource-type", "virtualMachines",
			"--all",
			"--output", "json")
		if err != nil {
			fmt.Printf("  ⚠  global SKU probe failed: %v (assuming all SKUs offered)\n", err)
		} else {
			var skus []azSkuEntry
			if err := json.Unmarshal(skuRaw, &skus); err != nil {
				fmt.Printf("  ⚠  global SKU parse failed: %v (assuming all SKUs offered)\n", err)
			} else {
				skuAvailByRegion = computeSkuAvail(skus)
			}
		}
	}

	// Build quotacache.File.
	qf := &quotacache.File{
		SubscriptionID:   acct.ID,
		SubscriptionName: acct.Name,
		GeneratedAt:      time.Now().UTC(),
		VMSizes:          make(map[string]quotacache.VMSizeInfo, len(quotacache.KnownVMSizes)),
		Regions:          make(map[string]quotacache.RegionData),
	}
	for name, info := range quotacache.KnownVMSizes {
		qf.VMSizes[name] = info
	}

	relevantFamilies := make(map[string]bool, len(quotacache.RelevantFamilies))
	for _, f := range quotacache.RelevantFamilies {
		relevantFamilies[f] = true
	}
	relevantSKUs := make(map[string]bool, len(quotacache.KnownVMSizes))
	for sku := range quotacache.KnownVMSizes {
		relevantSKUs[sku] = true
	}

	var failed []string
	for _, res := range results {
		r := quotacache.NormaliseRegion(res.region)
		if res.err != nil {
			failed = append(failed, res.region)
			fmt.Printf("  ⚠  %-20s  usage error: %v\n", res.region, res.err)
			continue
		}

		// Family quota.
		families := make(map[string]quotacache.FamilyQuota)
		for _, e := range res.usage {
			if !relevantFamilies[e.Name.Value] {
				continue
			}
			limit, _ := e.Limit.Int64()
			cur, _ := e.CurrentValue.Int64()
			families[e.Name.Value] = quotacache.FamilyQuota{
				Limit:        int(limit),
				CurrentValue: int(cur),
			}
		}

		// SKU availability from the global probe. If the global probe failed
		// entirely (empty map), assume all SKUs offered to avoid hard-blocking.
		skus := make(map[string]bool, len(relevantSKUs))
		for sku := range relevantSKUs {
			if len(skuAvailByRegion) == 0 {
				skus[sku] = true
				continue
			}
			perRegion := skuAvailByRegion[r]
			if perRegion == nil {
				skus[sku] = false
				continue
			}
			skus[sku] = perRegion[sku]
		}

		if len(families) > 0 || len(skus) > 0 {
			qf.Regions[r] = quotacache.RegionData{
				Families: families,
				SKUs:     skus,
			}
		}
	}

	if err := quotacache.Save(outPath, qf); err != nil {
		return fmt.Errorf("refresh-quota: save: %w", err)
	}

	fmt.Printf("\n✓ region-quota.yaml written: %s\n", outPath)
	fmt.Printf("  subscription : %s\n", qf.SubscriptionName)
	fmt.Printf("  regions OK   : %d / %d\n", len(qf.Regions), len(candidateRegionsForQuota))
	if len(failed) > 0 {
		fmt.Printf("  regions failed: %v\n", failed)
	}

	// Summary table.
	type row struct {
		region  string
		limit   int
		used    int
		avail   int
		f4Slots int
		f8Slots int
		f4OK    bool
		f8OK    bool
	}
	var rows []row
	for r, rdata := range qf.Regions {
		q := rdata.Families["standardFSv2Family"]
		avail := q.Limit - q.CurrentValue
		if avail < 0 {
			avail = 0
		}
		f4OK := rdata.SKUs == nil || rdata.SKUs["Standard_F4s_v2"]
		f8OK := rdata.SKUs == nil || rdata.SKUs["Standard_F8s_v2"]
		f4Slots := 0
		f8Slots := 0
		if f4OK {
			f4Slots = avail / 4
		}
		if f8OK {
			f8Slots = avail / 8
		}
		rows = append(rows, row{
			region:  r,
			limit:   q.Limit,
			used:    q.CurrentValue,
			avail:   avail,
			f4Slots: f4Slots,
			f8Slots: f8Slots,
			f4OK:    f4OK,
			f8OK:    f8OK,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].region < rows[j].region })

	fmt.Printf("\n%-22s  %6s  %6s  %6s  %12s  %12s\n",
		"REGION", "LIMIT", "USED", "AVAIL", "F4s_v2", "F8s_v2")
	fmt.Printf("%s\n", repeat("-", 76))
	for _, rw := range rows {
		f4Cell := fmt.Sprintf("%d", rw.f4Slots)
		if !rw.f4OK {
			f4Cell = "restricted"
		}
		f8Cell := fmt.Sprintf("%d", rw.f8Slots)
		if !rw.f8OK {
			f8Cell = "restricted"
		}
		fmt.Printf("%-22s  %6d  %6d  %6d  %12s  %12s\n",
			rw.region, rw.limit, rw.used, rw.avail, f4Cell, f8Cell)
	}
	fmt.Println()
	return nil
}

func repeat(s string, n int) string {
	out := make([]byte, n*len(s))
	for i := range out {
		out[i] = s[0]
	}
	return string(out)
}

// computeSkuAvail buckets the global `az vm list-skus` result by region.
// A SKU is considered available in a region when it is listed in that region's
// locations AND has no Location-scope restriction blocking the subscription.
// Zone-only restrictions are ignored — they don't prevent regional deployment.
func computeSkuAvail(skus []azSkuEntry) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, e := range skus {
		if e.ResourceType != "virtualMachines" {
			continue
		}
		if _, ok := quotacache.KnownVMSizes[e.Name]; !ok {
			continue
		}
		restrictedRegions := map[string]bool{}
		for _, r := range e.Restrictions {
			if r.Type != "Location" {
				continue
			}
			for _, v := range r.Values {
				restrictedRegions[quotacache.NormaliseRegion(v)] = true
			}
		}
		for _, loc := range e.Locations {
			norm := quotacache.NormaliseRegion(loc)
			if out[norm] == nil {
				out[norm] = map[string]bool{}
			}
			out[norm][e.Name] = !restrictedRegions[norm]
		}
	}
	return out
}
