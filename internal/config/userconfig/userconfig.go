// Package userconfig manages the operator's persistent ~/.ddos/config.yaml file,
// which stores the observability stack identifiers produced by `ddos init-observability`.
package userconfig

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// File is the on-disk representation of ~/.ddos/config.yaml.
type File struct {
	SubscriptionID string        `yaml:"subscriptionId"`
	TenantID       string        `yaml:"tenantId"`
	Observability  Observability `yaml:"observability"`
	SafetyNet      *SafetyNet    `yaml:"safetyNet,omitempty"`
}

// Observability holds the persistent monitoring stack identifiers.
type Observability struct {
	ResourceGroup       string `yaml:"resourceGroup"`
	Location            string `yaml:"location"`
	WorkspaceResourceID string `yaml:"workspaceResourceId"`
	WorkspaceName       string `yaml:"workspaceName"`
	DCEURL              string `yaml:"dceUrl"`
	DCEResourceID       string `yaml:"dceResourceId"`
	DCRImmutableID      string `yaml:"dcrImmutableId"`
	DCRResourceID       string `yaml:"dcrResourceId"`
	GrafanaURL          string `yaml:"grafanaUrl"`
	GrafanaResourceID   string `yaml:"grafanaResourceId"`
	PrometheusQueryURL  string `yaml:"prometheusQueryUrl"`
}

// SafetyNet records the subscription-level cleanup deployment, if installed.
type SafetyNet struct {
	ResourceGroup   string `yaml:"resourceGroup"`
	FunctionAppName string `yaml:"functionAppName"`
	DeployedAt      string `yaml:"deployedAt"`
}

// Path returns the absolute path to ~/.ddos/config.yaml.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".ddos", "config.yaml"), nil
}

// Load reads ~/.ddos/config.yaml. If it does not exist it returns an empty File
// with err == nil so callers can distinguish "fresh install" from corruption.
func Load() (*File, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &File{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &f, nil
}

// Save writes ~/.ddos/config.yaml, creating the parent directory if needed.
func (f *File) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	b, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return os.WriteFile(path, b, 0o600)
}
