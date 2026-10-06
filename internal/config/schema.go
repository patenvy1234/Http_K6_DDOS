// Package config defines the attack.yaml schema and validation rules.
package config

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"strings"
	"time"
)

// AllowedVMSizes enumerates the per-VM SKUs supported by the tool.
// Hard cap of 8 vCPU is enforced here: no other SKUs are permitted.
var AllowedVMSizes = map[string]int{
	"Standard_F4s_v2": 4,
	"Standard_F8s_v2": 8,
}

// AttackProfile is the root of attack.yaml.
type AttackProfile struct {
	Version         int             `yaml:"version"`
	RunName         string          `yaml:"runName"`
	Target          Target          `yaml:"target"`
	Endpoints       []Endpoint      `yaml:"endpoints"`
	TrafficShaping  TrafficShaping  `yaml:"trafficShaping"`
	Fleet           Fleet           `yaml:"fleet"`
	Observability   *Observability  `yaml:"observability,omitempty"`
	Lifecycle       Lifecycle       `yaml:"lifecycle"`
}

// Target is the global RPS / duration profile.
type Target struct {
	RPS              int `yaml:"rps"`
	DurationSeconds  int `yaml:"durationSeconds"`
	RampUpSeconds    int `yaml:"rampUpSeconds"`
	RampDownSeconds  int `yaml:"rampDownSeconds"`
}

// Pattern is one of the K6 scenario archetypes.
type Pattern string

const (
	PatternKeepaliveBurst Pattern = "keepalive-burst"
	PatternNewConnFlood   Pattern = "new-conn-flood"
)

// AllowedPatterns is the set of supported pattern strings.
var AllowedPatterns = map[Pattern]struct{}{
	PatternKeepaliveBurst: {},
	PatternNewConnFlood:   {},
}

// Endpoint is one HTTP target.
type Endpoint struct {
	ID           string            `yaml:"id"`
	Method       string            `yaml:"method"`
	URL          string            `yaml:"url"`
	Weight       float64           `yaml:"weight"`
	Pattern      Pattern           `yaml:"pattern"`
	BodyBytes    int               `yaml:"bodyBytes,omitempty"`
	Headers      map[string]string `yaml:"headers,omitempty"`
	// ResolveToIP pins the hostname in URL to a specific IP at the K6 layer.
	// The Host header and TLS SNI remain the original hostname; only TCP
	// connection destination is overridden. Useful for testing a specific
	// origin or VIP directly while bypassing DNS / CDN routing.
	ResolveToIP  string            `yaml:"resolveToIP,omitempty"`
}

// TrafficShaping toggles HTTP-level behaviour applied to every request.
type TrafficShaping struct {
	DiscardResponseBodies bool     `yaml:"discardResponseBodies"`
	HTTP2                 bool     `yaml:"http2"`
	InsecureSkipTLSVerify bool     `yaml:"insecureSkipTLSVerify"`
	UserAgents            []string `yaml:"userAgents,omitempty"`
}

// Fleet describes the Azure VMSS layout. Egress is always per-VM public IP
// (each VM gets its own Standard SKU IPv4 address; no NAT Gateway).
type Fleet struct {
	Regions             []string `yaml:"regions"`
	VMSize              string   `yaml:"vmSize"`
	// InitialVMCount == 0 means "auto-derive from ceil(target.rps / FixedPerVMRPS)".
	// Set explicitly only if you want to override the auto-computed value.
	InitialVMCount      int      `yaml:"initialVMCount"`
	MaxVMCount          int      `yaml:"maxVMCount"`
	UseSpot             bool     `yaml:"useSpot"`
	PerVmIpForPatterns  []Pattern `yaml:"perVmIpForPatterns,omitempty"`
}

// Observability is normally injected from ~/.ddos/config.yaml; can be overridden per profile.
type Observability struct {
	WorkspaceResourceID   string `yaml:"workspaceResourceId"`
	DCEURL                string `yaml:"dceUrl"`
	DCRImmutableID        string `yaml:"dcrImmutableId"`
	GrafanaURL            string `yaml:"grafanaUrl"`
	ScrapeIntervalSeconds int    `yaml:"scrapeIntervalSeconds"`
}

// Lifecycle controls run-time guard rails.
type Lifecycle struct {
	TTLMinutes          int  `yaml:"ttlMinutes"`
	CleanupOnInterrupt  bool `yaml:"cleanupOnInterrupt"`
}

// Validate runs schema + semantic checks. It returns the first error it encounters
// to keep messages readable; callers may call repeatedly after fixes.
func (p *AttackProfile) Validate() error {
	if p.Version != 1 {
		return fmt.Errorf("version: expected 1, got %d", p.Version)
	}
	if strings.TrimSpace(p.RunName) == "" {
		return fmt.Errorf("runName: must not be empty")
	}
	if !runNameRegexp.MatchString(p.RunName) {
		return fmt.Errorf("runName %q: must match %s (used in resource names)", p.RunName, runNameRegexp)
	}

	// Target.
	if p.Target.RPS <= 0 {
		return fmt.Errorf("target.rps: must be > 0")
	}
	if p.Target.RPS > 2_000_000 {
		return fmt.Errorf("target.rps: %d exceeds sanity ceiling of 2,000,000", p.Target.RPS)
	}
	if p.Target.DurationSeconds <= 0 {
		return fmt.Errorf("target.durationSeconds: must be > 0")
	}
	if p.Target.RampUpSeconds < 0 || p.Target.RampDownSeconds < 0 {
		return fmt.Errorf("target.ramp{Up,Down}Seconds: must be >= 0")
	}

	// Endpoints.
	if len(p.Endpoints) == 0 {
		return fmt.Errorf("endpoints: at least one endpoint is required")
	}
	seenIDs := map[string]struct{}{}
	for i, ep := range p.Endpoints {
		ctx := fmt.Sprintf("endpoints[%d]", i)
		if ep.ID == "" {
			return fmt.Errorf("%s.id: must not be empty", ctx)
		}
		if _, dup := seenIDs[ep.ID]; dup {
			return fmt.Errorf("%s.id %q: duplicated", ctx, ep.ID)
		}
		seenIDs[ep.ID] = struct{}{}
		if !validMethod(ep.Method) {
			return fmt.Errorf("%s.method %q: must be one of GET/POST/PUT/PATCH/DELETE/HEAD", ctx, ep.Method)
		}
		u, err := url.Parse(ep.URL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("%s.url %q: not a valid absolute URL", ctx, ep.URL)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("%s.url scheme %q: must be http or https", ctx, u.Scheme)
		}
		if isReservedExampleHost(u.Hostname()) {
			return fmt.Errorf("%s.url %q: example.com/net/org are documentation placeholders; replace with an explicitly authorized target", ctx, ep.URL)
		}
		if ep.Weight <= 0 || math.IsNaN(ep.Weight) || math.IsInf(ep.Weight, 0) {
			return fmt.Errorf("%s.weight: must be > 0", ctx)
		}
		if _, ok := AllowedPatterns[ep.Pattern]; !ok {
			return fmt.Errorf("%s.pattern %q: not a known pattern", ctx, ep.Pattern)
		}
		if ep.BodyBytes < 0 {
			return fmt.Errorf("%s.bodyBytes: must be >= 0", ctx)
		}
		if ep.BodyBytes > 16*1024*1024 {
			return fmt.Errorf("%s.bodyBytes: %d exceeds 16 MiB cap (per-request memory pressure)", ctx, ep.BodyBytes)
		}
		if ep.ResolveToIP != "" && net.ParseIP(ep.ResolveToIP) == nil {
			return fmt.Errorf("%s.resolveToIP %q: not a valid IP address", ctx, ep.ResolveToIP)
		}
	}

	// Fleet.
	vcpu, ok := AllowedVMSizes[p.Fleet.VMSize]
	if !ok {
		return fmt.Errorf("fleet.vmSize %q: must be one of %v (per-VM cap is 8 vCPU)", p.Fleet.VMSize, allowedVMSizeKeys())
	}
	// InitialVMCount == 0 means auto-derive: ceil(target.rps / FixedPerVMRPS) total VMs.
	if p.Fleet.InitialVMCount < 0 {
		return fmt.Errorf("fleet.initialVMCount: must be >= 0 (0 = auto-derived from target RPS)")
	}
	if p.Fleet.InitialVMCount > 0 && p.Fleet.MaxVMCount < p.Fleet.InitialVMCount {
		return fmt.Errorf("fleet.maxVMCount (%d): must be >= initialVMCount (%d)", p.Fleet.MaxVMCount, p.Fleet.InitialVMCount)
	}
	if p.Fleet.MaxVMCount > 500 {
		return fmt.Errorf("fleet.maxVMCount: %d exceeds sanity ceiling of 500", p.Fleet.MaxVMCount)
	}
	// fleet.regions: []  →  auto-expand to all regions with available quota.
	// resolveRegions in runner.go fills the slice before deployment begins.

	// Headroom warning is informational, not an error — total capacity check:
	// 15k RPS/vCPU is the conservative baseline.
	maxRPS := p.Fleet.MaxVMCount * vcpu * 15_000
	if maxRPS < p.Target.RPS {
		return fmt.Errorf("fleet.maxVMCount (%d) * %d vCPU * 15k RPS = %d cannot reach target RPS %d; raise maxVMCount or vmSize",
			p.Fleet.MaxVMCount, vcpu, maxRPS, p.Target.RPS)
	}

	// Lifecycle.
	if p.Lifecycle.TTLMinutes <= 0 {
		return fmt.Errorf("lifecycle.ttlMinutes: must be > 0")
	}
	totalSec := p.Target.RampUpSeconds + p.Target.DurationSeconds + p.Target.RampDownSeconds
	if time.Duration(p.Lifecycle.TTLMinutes)*time.Minute < time.Duration(totalSec)*time.Second {
		return fmt.Errorf("lifecycle.ttlMinutes (%d min): shorter than ramp+duration+ramp (%ds)", p.Lifecycle.TTLMinutes, totalSec)
	}

	return nil
}

func validMethod(m string) bool {
	switch strings.ToUpper(m) {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
		return true
	}
	return false
}

func isReservedExampleHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, reserved := range []string{"example.com", "example.net", "example.org"} {
		if host == reserved || strings.HasSuffix(host, "."+reserved) {
			return true
		}
	}
	return false
}

func allowedVMSizeKeys() []string {
	keys := make([]string, 0, len(AllowedVMSizes))
	for k := range AllowedVMSizes {
		keys = append(keys, k)
	}
	return keys
}
