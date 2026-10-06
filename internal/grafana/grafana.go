// Package grafana deploys Grafana dashboards to an Azure Managed Grafana instance
// by calling `az grafana dashboard import` for each embedded JSON file.
package grafana

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/azcli"
)

// DeployDashboards imports all Grafana dashboard JSON files from dashFS into
// the Azure Managed Grafana instance identified by grafanaName + resourceGroup.
// dashFS should contain .json files at its root (use fs.Sub if needed).
// The function is idempotent — it overwrites dashboards with the same UID.
func DeployDashboards(ctx context.Context, az *azcli.Runner, grafanaName, resourceGroup string, dashFS fs.FS) error {
	entries, err := fs.ReadDir(dashFS, ".")
	if err != nil {
		return fmt.Errorf("grafana: read dashboards: %w", err)
	}
	var errs []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := fs.ReadFile(dashFS, e.Name())
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: read: %v", e.Name(), err))
			continue
		}
		if err := importDashboard(ctx, az, grafanaName, resourceGroup, e.Name(), data); err != nil {
			errs = append(errs, fmt.Sprintf("%s: import: %v", e.Name(), err))
		} else {
			fmt.Printf("  ✓ dashboard imported: %s\n", e.Name())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("grafana: %d import error(s): %s", len(errs), strings.Join(errs, "; "))
	}
	return nil
}

// importDashboard writes the dashboard JSON to a temp file and calls
// `az grafana dashboard import`.
func importDashboard(ctx context.Context, az *azcli.Runner, grafanaName, rg, filename string, data []byte) error {
	tmp, err := os.CreateTemp("", "ddos-dash-*-"+filepath.Base(filename))
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	_, err = az.Run(ctx,
		"grafana", "dashboard", "import",
		"--name", grafanaName,
		"--resource-group", rg,
		"--definition", tmp.Name(),
		"--overwrite", "true",
	)
	return err
}
