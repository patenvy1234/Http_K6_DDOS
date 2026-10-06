// Package quotacache reads and queries the static region-quota.yaml file that
// lives in the repo root. It provides quota-aware region selection and
// round-robin allocation for the load-test fleet.
package quotacache

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// KnownVMSizes maps every supported ddos VM size to its family and vCPU count.
// Add entries here if new sizes are allowed in the future.
var KnownVMSizes = map[string]VMSizeInfo{
	"Standard_F4s_v2": {Family: "standardFSv2Family", VCPUsPerVM: 4},
	"Standard_F8s_v2": {Family: "standardFSv2Family", VCPUsPerVM: 8},
}

// Families we always collect when running refresh-quota.
var RelevantFamilies = []string{"standardFSv2Family"}

// ------------------------------------------------------------------
// Types
// ------------------------------------------------------------------

// File is the deserialized representation of region-quota.yaml.
type File struct {
	SubscriptionID   string                `yaml:"subscriptionId"`
	SubscriptionName string                `yaml:"subscriptionName"`
	GeneratedAt      time.Time             `yaml:"generatedAt"`
	VMSizes          map[string]VMSizeInfo `yaml:"vmSizes"`
	Regions          map[string]RegionData `yaml:"regions"`
}

// RegionData holds the per-region quota and SKU availability snapshot.
type RegionData struct {
	Families map[string]FamilyQuota `yaml:"families"`
	// SKUs records whether each ddos-supported VM size is actually offerable
	// (not subscription-restricted) in this region. Map value is true when
	// the SKU has no Location-scope restriction.
	SKUs map[string]bool `yaml:"skus,omitempty"`
}

// VMSizeInfo describes one VM size that ddos can deploy.
type VMSizeInfo struct {
	Family     string `yaml:"family"`
	VCPUsPerVM int    `yaml:"vcpusPerVm"`
}

// FamilyQuota holds the subscription quota for one VM family in one region.
type FamilyQuota struct {
	Limit        int `yaml:"limit"`
	CurrentValue int `yaml:"currentValue"`
}

// RegionAlloc is returned by PlanAllocation: one entry per region with how
// many VMs should be deployed there.
type RegionAlloc struct {
	Region string
	Count  int
}

// ------------------------------------------------------------------
// Load / Save
// ------------------------------------------------------------------

// Load reads a region-quota.yaml file from the given path.
// Returns an error if the file doesn't exist or can't be parsed.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("quotacache: parse %s: %w", path, err)
	}
	return &f, nil
}

// Save writes a File as YAML to the given path.
func Save(path string, f *File) error {
	data, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("quotacache: marshal: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

// ------------------------------------------------------------------
// Helpers
// ------------------------------------------------------------------

// NormaliseRegion returns a consistent, lowercase region name with no spaces.
func NormaliseRegion(r string) string {
	return strings.ToLower(strings.ReplaceAll(r, " ", ""))
}

// ------------------------------------------------------------------
// Query methods
// ------------------------------------------------------------------

// AvailableVMs returns how many VMs of vmSize can still be deployed in region
// given the last-recorded quota snapshot. Returns 0 if region/size unknown,
// if the SKU is subscription-restricted in the region, or if family quota
// is exhausted.
func (f *File) AvailableVMs(region, vmSize string) int {
	sizeInfo, ok := f.VMSizes[vmSize]
	if !ok {
		sizeInfo, ok = KnownVMSizes[vmSize]
		if !ok {
			return 0
		}
	}
	if sizeInfo.VCPUsPerVM <= 0 {
		return 0
	}
	rdata, ok := f.Regions[NormaliseRegion(region)]
	if !ok {
		return 0
	}
	// SKU availability gate: if SKU map is populated and explicitly says
	// the SKU is unavailable, treat region as having zero capacity even if
	// family quota is plentiful.
	if rdata.SKUs != nil {
		if avail, present := rdata.SKUs[vmSize]; present && !avail {
			return 0
		}
	}
	quota, ok := rdata.Families[sizeInfo.Family]
	if !ok {
		return 0
	}
	avail := quota.Limit - quota.CurrentValue
	if avail <= 0 {
		return 0
	}
	return avail / sizeInfo.VCPUsPerVM
}

// RegionsByAvailability returns all regions that have at least one available
// VM slot for vmSize, sorted by available count descending.
// Regions in exclude are omitted.
func (f *File) RegionsByAvailability(vmSize string, exclude map[string]bool) []string {
	type item struct {
		region string
		avail  int
	}
	var items []item
	for r := range f.Regions {
		if exclude != nil && exclude[r] {
			continue
		}
		if a := f.AvailableVMs(r, vmSize); a > 0 {
			items = append(items, item{r, a})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].avail > items[j].avail
	})
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.region
	}
	return out
}

// PlanAllocation distributes needed VMs across fresh (unused, not excluded)
// regions using a round-robin strategy, respecting each region's available
// quota as a hard cap.
//
// Returns an empty slice if no eligible regions exist.
func (f *File) PlanAllocation(vmSize string, needed int, used, excluded map[string]bool) []RegionAlloc {
	if needed <= 0 {
		return nil
	}

	// Eligible = has available quota + not already in use + not excluded.
	type candidate struct {
		region    string
		availCap  int
		allocated int
	}
	eligible := f.RegionsByAvailability(vmSize, excluded)
	var candidates []*candidate
	for _, r := range eligible {
		if used[r] {
			continue
		}
		candidates = append(candidates, &candidate{
			region:   r,
			availCap: f.AvailableVMs(r, vmSize),
		})
	}
	if len(candidates) == 0 {
		return nil
	}

	// Round-robin: one VM per region per cycle until needed satisfied or all
	// regions hit their quota cap.
	allocated := 0
	for allocated < needed {
		progressed := false
		for _, c := range candidates {
			if allocated >= needed {
				break
			}
			if c.allocated >= c.availCap {
				continue
			}
			c.allocated++
			allocated++
			progressed = true
		}
		if !progressed {
			break // all candidates exhausted
		}
	}

	var out []RegionAlloc
	for _, c := range candidates {
		if c.allocated > 0 {
			out = append(out, RegionAlloc{Region: c.region, Count: c.allocated})
		}
	}
	return out
}
