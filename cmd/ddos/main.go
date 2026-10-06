// Package main is the entrypoint for the ddos CLI.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Version is overridden via -ldflags at build time.
var Version = "dev"

func main() {
	root := &cobra.Command{
		Use:           "ddos",
		Short:         "Azure-backed K6 load generator (target: 1M RPS).",
		Long:          "ddos deploys K6 worker fleets across Azure regions, drives traffic against\nconfigured endpoints, streams metrics to Azure Monitor managed Prometheus, and\ntears everything down when the run ends.",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newInitObservabilityCmd(),
		newInitSafetyNetCmd(),
		newRefreshSkuCacheCmd(),
		newRefreshQuotaCmd(),
		newValidateCmd(),
		newRunCmd(),
		newDestroyCmd(),
		newObserveCmd(),
		newDoctorCmd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
