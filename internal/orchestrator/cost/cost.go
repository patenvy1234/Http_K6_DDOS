// Package cost produces conservative planning estimates for a load-test run.
package cost

import (
	"fmt"
	"math"
	"strings"

	"github.com/patenvy1234/Http_K6_DDOS/internal/config"
	"github.com/patenvy1234/Http_K6_DDOS/internal/orchestrator/profilegen"
)

const (
	bytesPerGiB                = 1024 * 1024 * 1024
	egressUSDPerGiB            = 0.087
	publicIPUSDPerHour         = 0.004
	keepaliveOverheadBytes     = 400
	newConnectionOverheadBytes = 3_000
	defaultWriteBodyBytes      = 512
)

var vmPrices = map[string]float64{
	"Standard_F4s_v2": 0.169,
	"Standard_F8s_v2": 0.338,
}

// Estimate is a conservative planning estimate, not an Azure billing quote.
// Fleet costs assume maxVMCount is allocated for the full lifecycle TTL.
type Estimate struct {
	TotalUSD            float64 `json:"totalUsd"`
	ComputeUSD          float64 `json:"computeUsd"`
	PublicIPUSD         float64 `json:"publicIpUsd"`
	EgressUSD           float64 `json:"egressUsd"`
	EgressGiB           float64 `json:"egressGiB"`
	AverageRequestBytes float64 `json:"averageRequestBytes"`
	WorkloadSeconds     int     `json:"workloadSeconds"`
	MaxTrafficRPS       int     `json:"maxTrafficRps"`
	TrafficSeconds      float64 `json:"trafficSeconds"`
	MaxVMCount          int     `json:"maxVmCount"`
	VMHours             float64 `json:"vmHours"`
}

// EstimateMax calculates a conservative run cost using reference prices.
func EstimateMax(profile *config.AttackProfile) (Estimate, error) {
	if profile == nil {
		return Estimate{}, fmt.Errorf("cost: profile is nil")
	}
	if profile.Target.RPS <= 0 || profile.Fleet.MaxVMCount <= 0 || profile.Lifecycle.TTLMinutes <= 0 {
		return Estimate{}, fmt.Errorf("cost: target RPS, maximum VM count, and TTL must be positive")
	}
	price, ok := vmPrices[profile.Fleet.VMSize]
	if !ok {
		return Estimate{}, fmt.Errorf("cost: no planning price for VM size %q", profile.Fleet.VMSize)
	}

	averageBytes, err := averageRequestBytes(profile.Endpoints)
	if err != nil {
		return Estimate{}, err
	}
	workloadSeconds := profile.Target.RampUpSeconds + profile.Target.DurationSeconds + profile.Target.RampDownSeconds
	maxTrafficRPS := max(profile.Target.RPS, profile.Fleet.MaxVMCount*profilegen.FixedPerVMRPS)
	trafficSeconds := float64(profile.Lifecycle.TTLMinutes) * 60
	requestCount := float64(maxTrafficRPS) * trafficSeconds
	// Only bytes sent from the Azure workers are outbound data transfer.
	// Target responses flow into Azure and are therefore not egress charges.
	egressGiB := requestCount * averageBytes / bytesPerGiB

	vmHours := float64(profile.Fleet.MaxVMCount) * float64(profile.Lifecycle.TTLMinutes) / 60
	computeUSD := vmHours * price
	publicIPUSD := vmHours * publicIPUSDPerHour
	egressUSD := egressGiB * egressUSDPerGiB

	return Estimate{
		TotalUSD:            computeUSD + publicIPUSD + egressUSD,
		ComputeUSD:          computeUSD,
		PublicIPUSD:         publicIPUSD,
		EgressUSD:           egressUSD,
		EgressGiB:           egressGiB,
		AverageRequestBytes: averageBytes,
		WorkloadSeconds:     workloadSeconds,
		MaxTrafficRPS:       maxTrafficRPS,
		TrafficSeconds:      trafficSeconds,
		MaxVMCount:          profile.Fleet.MaxVMCount,
		VMHours:             vmHours,
	}, nil
}

// Enforce returns the estimate and rejects a run above maxUSD. A zero limit
// explicitly disables enforcement; negative limits are invalid.
func Enforce(profile *config.AttackProfile, maxUSD float64) (Estimate, error) {
	if maxUSD < 0 || math.IsNaN(maxUSD) || math.IsInf(maxUSD, 0) {
		return Estimate{}, fmt.Errorf("cost: max cost must be finite and >= 0")
	}
	estimate, err := EstimateMax(profile)
	if err != nil {
		return Estimate{}, err
	}
	if maxUSD > 0 && estimate.TotalUSD > maxUSD {
		return estimate, fmt.Errorf(
			"estimated planning cost $%.2f exceeds the $%.2f limit (egress $%.2f, compute $%.2f, public IPs $%.2f)",
			estimate.TotalUSD, maxUSD, estimate.EgressUSD, estimate.ComputeUSD, estimate.PublicIPUSD,
		)
	}
	return estimate, nil
}

func averageRequestBytes(endpoints []config.Endpoint) (float64, error) {
	var maxWeight float64
	for _, endpoint := range endpoints {
		if endpoint.Weight <= 0 || math.IsNaN(endpoint.Weight) || math.IsInf(endpoint.Weight, 0) {
			return 0, fmt.Errorf("cost: endpoint weights must be finite and positive")
		}
		maxWeight = math.Max(maxWeight, endpoint.Weight)
	}
	var weightedBytes float64
	var totalWeight float64
	for _, endpoint := range endpoints {
		requestBytes := keepaliveOverheadBytes
		if endpoint.Pattern == config.PatternNewConnFlood {
			requestBytes = newConnectionOverheadBytes
		}
		requestBytes += len(endpoint.URL)
		for name, value := range endpoint.Headers {
			requestBytes += len(name) + len(value) + 4
		}
		if isWriteMethod(endpoint.Method) {
			bodyBytes := endpoint.BodyBytes
			if bodyBytes == 0 {
				bodyBytes = defaultWriteBodyBytes
			}
			requestBytes += max(bodyBytes, len(`{"payload":"a","seq":0}`))
		}
		weight := endpoint.Weight / maxWeight
		weightedBytes += weight * float64(requestBytes)
		totalWeight += weight
	}
	if totalWeight == 0 {
		return 0, fmt.Errorf("cost: endpoints must have positive total weight")
	}
	return weightedBytes / totalWeight, nil
}

func isWriteMethod(method string) bool {
	switch strings.ToUpper(method) {
	case "POST", "PUT", "PATCH":
		return true
	default:
		return false
	}
}
