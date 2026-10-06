package main

import (
	"fmt"
	"net/url"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/patenvy1234/Http_K6_DDOS/internal/config/userconfig"
)

func newObserveCmd() *cobra.Command {
	var runID string
	cmd := &cobra.Command{
		Use:   "observe",
		Short: "Open Azure Managed Grafana in your browser.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := userconfig.Load()
			if err != nil {
				return err
			}
			if cfg.Observability.GrafanaURL == "" {
				return fmt.Errorf("no Grafana URL in ~/.ddos/config.yaml — run `ddos init-observability` first")
			}
			target := cfg.Observability.GrafanaURL
			if runID != "" {
				u, err := url.Parse(target)
				if err == nil {
					q := u.Query()
					q.Set("var-runId", runID)
					u.RawQuery = q.Encode()
					target = u.String()
				}
			}
			fmt.Printf("→ Opening %s\n", target)
			return openBrowser(target)
		},
	}
	cmd.Flags().StringVar(&runID, "run-id", "", "Pre-filter dashboards by run ID")
	return cmd
}

func openBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		cmd = exec.Command("open", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}
