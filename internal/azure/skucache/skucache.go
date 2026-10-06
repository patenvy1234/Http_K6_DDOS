// Package skucache reads and writes the on-disk SKU availability cache
// (~/.ddos/sku-availability.json) used by `ddos run` to pick deployable
// regions without making a live `az vm list-skus` call every time.
//
// The cache is populated by `ddos refresh-sku-cache` (one slow call to
// `az vm list-skus --resource-type virtualMachines`) and then used as the
// source of truth by the runner. Dynamic spot capacity exhaustion is not
// captured here — that is still handled by the runner's retry-on-error
// layer when a deploy actually fails.
package skucache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// File is the JSON document persisted to ~/.ddos/sku-availability.json.
type File struct {
	SubscriptionID   string             `json:"subscriptionId"`
	SubscriptionName string             `json:"subscriptionName,omitempty"`
	GeneratedAt      time.Time          `json:"generatedAt"`
	AllRegions       []string           `json:"allRegions"`
	Skus             map[string]SkuInfo `json:"skus"`
}

// SkuInfo lists regions where a SKU is offered to the subscription with no
// blocking restriction (Available) and those where it is offered but blocked
// (Restricted, mapped to the reason).
type SkuInfo struct {
	Available  []string          `json:"available"`
	Restricted map[string]string `json:"restricted,omitempty"`
}

// DefaultPath returns ~/.ddos/sku-availability.json.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".ddos", "sku-availability.json"), nil
}

// Load reads the cache from disk. Returns os.ErrNotExist if the file is
// missing; callers should treat that as "run `ddos refresh-sku-cache`".
func Load(path string) (*File, error) {
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return &f, nil
}

// Save writes the cache to disk, creating the parent directory if needed.
func Save(path string, f *File) error {
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return err
		}
		path = p
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	return os.WriteFile(path, b, 0o644)
}

// AvailableRegions returns the sorted list of regions where the SKU is
// available without restrictions. Returns nil if the SKU was never seen.
func (f *File) AvailableRegions(sku string) []string {
	info, ok := f.Skus[sku]
	if !ok {
		return nil
	}
	out := append([]string(nil), info.Available...)
	sort.Strings(out)
	return out
}

// RestrictionReason returns the cached restriction reason for (sku, region),
// or "" if the SKU is available there or not present.
func (f *File) RestrictionReason(sku, region string) string {
	info, ok := f.Skus[sku]
	if !ok {
		return ""
	}
	return info.Restricted[region]
}

// AzRestriction is one entry of the `restrictions` array on a SKU.
type AzRestriction struct {
	Type            string   `json:"type"`
	ReasonCode      string   `json:"reasonCode"`
	Values          []string `json:"values"`
	RestrictionInfo struct {
		Locations []string `json:"locations"`
		Zones     []string `json:"zones"`
	} `json:"restrictionInfo"`
}

// AzSkuEntry mirrors the JSON shape emitted by `az vm list-skus
// --resource-type virtualMachines --output json`. Only fields used to derive
// availability are decoded.
type AzSkuEntry struct {
	Name         string          `json:"name"`
	ResourceType string          `json:"resourceType"`
	Locations    []string        `json:"locations"`
	Restrictions []AzRestriction `json:"restrictions"`
}

// BuildFromAzEntries turns the raw `az vm list-skus` output into a populated
// File. Only VM-type SKUs are considered. Each entry generally represents one
// (SKU, region) pair; restrictions whose Values include the entry's region
// flag that region as unavailable for the subscription. Zone-only
// restrictions are ignored because the SKU is still deployable in the region
// from at least one zone.
func BuildFromAzEntries(
	subscriptionID, subscriptionName string,
	entries []AzSkuEntry,
) *File {
	skus := map[string]SkuInfo{}
	regionSet := map[string]bool{}
	for _, e := range entries {
		if e.ResourceType != "" && e.ResourceType != "virtualMachines" {
			continue
		}
		if e.Name == "" {
			continue
		}
		for _, raw := range e.Locations {
			region := NormaliseRegion(raw)
			if region == "" {
				continue
			}
			regionSet[region] = true
			info := skus[e.Name]
			if info.Restricted == nil {
				info.Restricted = map[string]string{}
			}
			blocked, reason := isRegionBlocked(e.Restrictions, region)
			if blocked {
				info.Restricted[region] = reason
			} else {
				info.Available = append(info.Available, region)
			}
			skus[e.Name] = info
		}
	}
	for k, v := range skus {
		sort.Strings(v.Available)
		v.Available = dedupe(v.Available)
		if len(v.Restricted) == 0 {
			v.Restricted = nil
		}
		skus[k] = v
	}
	allRegions := make([]string, 0, len(regionSet))
	for r := range regionSet {
		allRegions = append(allRegions, r)
	}
	sort.Strings(allRegions)
	return &File{
		SubscriptionID:   subscriptionID,
		SubscriptionName: subscriptionName,
		GeneratedAt:      time.Now().UTC(),
		AllRegions:       allRegions,
		Skus:             skus,
	}
}

func isRegionBlocked(restrictions []AzRestriction, region string) (bool, string) {
	for _, r := range restrictions {
		matchedRegion := false
		for _, v := range r.Values {
			if NormaliseRegion(v) == region {
				matchedRegion = true
				break
			}
		}
		for _, v := range r.RestrictionInfo.Locations {
			if NormaliseRegion(v) == region {
				matchedRegion = true
				break
			}
		}
		if !matchedRegion && len(r.Values) == 0 && len(r.RestrictionInfo.Locations) == 0 {
			matchedRegion = true
		}
		if matchedRegion && r.Type != "Zone" {
			reason := r.ReasonCode
			if reason == "" {
				reason = r.Type
			}
			return true, reason
		}
	}
	return false, ""
}

// NormaliseRegion lower-cases and strips spaces so values like "East US",
// "eastus", and "eastUS" all compare equal.
func NormaliseRegion(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, " ", ""))
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	out := xs[:0]
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
