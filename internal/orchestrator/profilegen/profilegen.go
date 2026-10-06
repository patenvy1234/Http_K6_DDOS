// Package profilegen converts an AttackProfile + RunSpec into the profile.json
// document that every k6 worker reads from /etc/ddos/profile.json.
//
// Every VM always targets FixedPerVMRPS regardless of fleet size. The
// controller derives the required VM count as ceil(targetRPS / FixedPerVMRPS)
// and spreads them evenly across regions.
package profilegen

import (
	"encoding/json"
	"fmt"

	"github.com/patenvy1234/Http_K6_DDOS/internal/config"
	"github.com/patenvy1234/Http_K6_DDOS/internal/orchestrator/runspec"
)

// FixedPerVMRPS is the constant per-VM RPS target every k6 worker is configured
// to generate. Fleet size is auto-derived: ceil(targetRPS / FixedPerVMRPS).
//
// 5,000 RPS matches the empirically measured sustained ceiling of an
// F4s_v2 (4 vCPU) under both keepalive-burst and new-conn-flood patterns.
// Keeping aspiration at or below this ceiling eliminates the CPU saturation
// that causes k6 to drop ~1,200 iter/s even when user target is fully met.
const FixedPerVMRPS = 5_000

const MaxVMSSCapacity = 200

func PlanInitialFleet(profile *config.AttackProfile) (perRegion, regionCount int, err error) {
	if profile == nil || profile.Target.RPS <= 0 || profile.Fleet.MaxVMCount <= 0 || profile.Fleet.InitialVMCount < 0 {
		return 0, 0, fmt.Errorf("fleet sizing requires positive target RPS and VM ceiling, and a non-negative initial count")
	}
	regionCount = len(profile.Fleet.Regions)
	if regionCount == 0 {
		return 0, 0, fmt.Errorf("resolve fleet regions before computing initial capacity")
	}
	if profile.Fleet.InitialVMCount > 0 {
		perRegion = profile.Fleet.InitialVMCount
		if perRegion > MaxVMSSCapacity || perRegion > profile.Fleet.MaxVMCount/regionCount {
			return 0, 0, fmt.Errorf("initial fleet exceeds maxVMCount (%d) or the per-region limit (%d)", profile.Fleet.MaxVMCount, MaxVMSSCapacity)
		}
		return perRegion, regionCount, nil
	}
	requiredVMs := (profile.Target.RPS-1)/FixedPerVMRPS + 1
	if requiredVMs > profile.Fleet.MaxVMCount {
		return 0, 0, fmt.Errorf("target requires %d initial VMs at %d RPS/VM but maxVMCount is %d", requiredVMs, FixedPerVMRPS, profile.Fleet.MaxVMCount)
	}
	for regionCount = min(regionCount, requiredVMs); regionCount > 0; regionCount-- {
		perRegion = (requiredVMs-1)/regionCount + 1
		if perRegion <= MaxVMSSCapacity && perRegion <= profile.Fleet.MaxVMCount/regionCount {
			return perRegion, regionCount, nil
		}
	}
	return 0, 0, fmt.Errorf("cannot fit a uniformly sized initial fleet under maxVMCount (%d); add regions or adjust the ceiling", profile.Fleet.MaxVMCount)
}

// VMProfileis the JSON structure written to /etc/ddos/profile.json.
// Field names are snake_case to match what k6/lib/profile.js expects.
type VMProfile struct {
	RunID           string     `json:"run_id"`
	RunName         string     `json:"run_name"`
	StartAt         string     `json:"start_at"`
	TargetRPS       int        `json:"target_rps"`
	RampUpSeconds   int        `json:"ramp_up_seconds"`
	DurationSeconds int        `json:"duration_seconds"`
	RampDownSeconds int        `json:"ramp_down_seconds"`
	Pattern         string     `json:"pattern"`
	Endpoints       []Endpoint `json:"endpoints"`
}

// Endpoint mirrors the profile.js endpoint schema.
type Endpoint struct {
	ID          string            `json:"id"`
	URL         string            `json:"url"`
	Method      string            `json:"method"`
	Weight      float64           `json:"weight"`
	Headers     map[string]string `json:"headers,omitempty"`
	BodyBytes   int               `json:"body_bytes,omitempty"`
	ConnMode    string            `json:"conn_mode,omitempty"`     // "keepalive" | "new-conn"
	ResolveToIP string            `json:"resolve_to_ip,omitempty"` // override DNS to this IP
}

// Generate builds a VMProfile for a single worker VM.
//
// Every VM targets exactly FixedPerVMRPS. The fleet size is derived by the
// caller (runner) as ceil(totalRPS / FixedPerVMRPS) spread across regions.
func Generate(p *config.AttackProfile, spec *runspec.RunSpec) ([]byte, error) {
	perVMRPS := FixedPerVMRPS
	pattern, err := resolvePattern(p)
	if err != nil {
		return nil, err
	}

	endpoints := make([]Endpoint, len(p.Endpoints))
	for i, ep := range p.Endpoints {
		endpoints[i] = Endpoint{
			ID:          ep.ID,
			URL:         ep.URL,
			Method:      ep.Method,
			Weight:      ep.Weight,
			Headers:     ep.Headers,
			BodyBytes:   ep.BodyBytes,
			ConnMode:    connMode(ep.Pattern),
			ResolveToIP: ep.ResolveToIP,
		}
	}

	vmp := VMProfile{
		RunID:           spec.RunSuffix,
		RunName:         spec.RunName,
		StartAt:         spec.StartAt.Format("2006-01-02T15:04:05Z"),
		TargetRPS:       perVMRPS,
		RampUpSeconds:   p.Target.RampUpSeconds,
		DurationSeconds: p.Target.DurationSeconds,
		RampDownSeconds: p.Target.RampDownSeconds,
		Pattern:         string(pattern),
		Endpoints:       endpoints,
	}

	return json.MarshalIndent(vmp, "", "  ")
}

// resolvePattern returns the single k6 pattern shared by all endpoints.
// All endpoints in a run must share the same pattern; mixed-pattern profiles
// are rejected so the per-VM k6 process can run one constant-arrival-rate
// scenario with correctly-sized VU budgets.
func resolvePattern(p *config.AttackProfile) (config.Pattern, error) {
	if len(p.Endpoints) == 0 {
		return config.PatternKeepaliveBurst, nil
	}
	first := p.Endpoints[0].Pattern
	for i, ep := range p.Endpoints[1:] {
		if ep.Pattern != first {
			return "", fmt.Errorf("profilegen: endpoints must all share one pattern; endpoint[0]=%q but endpoint[%d]=%q", first, i+1, ep.Pattern)
		}
	}
	return first, nil
}

// connMode maps a per-endpoint pattern to the k6 conn_mode field.
// Only new-conn-flood needs explicit "new-conn"; everything else keeps alive.
func connMode(p config.Pattern) string {
	if p == config.PatternNewConnFlood {
		return "new-conn"
	}
	return ""
}
