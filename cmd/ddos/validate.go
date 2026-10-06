package main

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/patenvy1234/Http_K6_DDOS/internal/config"
	"github.com/patenvy1234/Http_K6_DDOS/internal/orchestrator/cost"
	"github.com/patenvy1234/Http_K6_DDOS/internal/orchestrator/profilegen"
)

func newValidateCmd() *cobra.Command {
	var maxCost float64
	var outputJSON bool
	cmd := &cobra.Command{
		Use:   "validate <attack.yaml>",
		Short: "Validate an attack profile and estimate its maximum run cost.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]
			profile, err := config.LoadFile(path)
			if err != nil {
				return err
			}
			if err := profile.Validate(); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			initialVMCount := 0
			if len(profile.Fleet.Regions) > 0 {
				perRegion, regionCount, err := profilegen.PlanInitialFleet(profile)
				if err != nil {
					return fmt.Errorf("%s: fleet sizing: %w", path, err)
				}
				initialVMCount = perRegion * regionCount
			}
			estimate, err := cost.Enforce(profile, maxCost)
			if err != nil {
				return fmt.Errorf("%s: cost guard: %w", path, err)
			}

			if outputJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(validationOutput{
					Valid:                true,
					Profile:              path,
					RunName:              profile.RunName,
					TargetRPS:            profile.Target.RPS,
					EndpointCount:        len(profile.Endpoints),
					Patterns:             summarizePatterns(profile),
					Regions:              profile.Fleet.Regions,
					VMSize:               profile.Fleet.VMSize,
					MaxVMCount:           profile.Fleet.MaxVMCount,
					InitialVMCount:       initialVMCount,
					UseSpot:              profile.Fleet.UseSpot,
					EstimatedMaximumCost: estimate,
				})
			}
			fmt.Printf("✓ %s\n", path)
			fmt.Printf("  Run name:  %s\n", profile.RunName)
			fmt.Printf("  Target:    %d RPS for %ds (+%ds ramp)\n",
				profile.Target.RPS, profile.Target.DurationSeconds, profile.Target.RampUpSeconds)
			fmt.Printf("  Endpoints: %d (patterns: %s)\n", len(profile.Endpoints), summarizePatterns(profile))

			initStr := fleetInitStr(profile)
			fmt.Printf("  Fleet:     %s-%d × %s across %v (spot=%t)\n",
				initStr, profile.Fleet.MaxVMCount, profile.Fleet.VMSize,
				profile.Fleet.Regions, profile.Fleet.UseSpot)
			fmt.Printf("  Cost max:  $%.2f (egress $%.2f + compute $%.2f + public IPs $%.2f)\n",
				estimate.TotalUSD, estimate.EgressUSD, estimate.ComputeUSD, estimate.PublicIPUSD)
			return nil
		},
	}
	cmd.Flags().Float64Var(&maxCost, "max-cost", 0, "Fail if estimated maximum cost (USD) exceeds this value; 0 only reports the estimate")
	cmd.Flags().BoolVar(&outputJSON, "json", false, "Emit a machine-readable validation and cost summary")
	return cmd
}

type validationOutput struct {
	Valid                bool          `json:"valid"`
	Profile              string        `json:"profile"`
	RunName              string        `json:"runName"`
	TargetRPS            int           `json:"targetRps"`
	EndpointCount        int           `json:"endpointCount"`
	Patterns             string        `json:"patterns"`
	Regions              []string      `json:"regions"`
	VMSize               string        `json:"vmSize"`
	MaxVMCount           int           `json:"maxVmCount"`
	InitialVMCount       int           `json:"initialTotalVmCount,omitempty"`
	UseSpot              bool          `json:"useSpot"`
	EstimatedMaximumCost cost.Estimate `json:"estimatedMaximumCost"`
}

// fleetInitStr returns a human-readable initial VM count string.
// When InitialVMCount is 0 it shows the auto-derived value with annotation.
func fleetInitStr(p *config.AttackProfile) string {
	if len(p.Fleet.Regions) == 0 {
		return "pending region resolution"
	}
	perRegion, regions, err := profilegen.PlanInitialFleet(p)
	if err != nil {
		return "invalid initial capacity"
	}
	return fmt.Sprintf("%d/region (%d total across %d regions @ %d RPS/VM)", perRegion, perRegion*regions, regions, profilegen.FixedPerVMRPS)
}

func summarizePatterns(p *config.AttackProfile) string {
	seen := map[config.Pattern]int{}
	for _, ep := range p.Endpoints {
		seen[ep.Pattern]++
	}
	patterns := make([]string, 0, len(seen))
	for pattern := range seen {
		patterns = append(patterns, string(pattern))
	}
	sort.Strings(patterns)
	out := ""
	for _, pattern := range patterns {
		if out != "" {
			out += ", "
		}
		out += fmt.Sprintf("%s×%d", pattern, seen[config.Pattern(pattern)])
	}
	return out
}
