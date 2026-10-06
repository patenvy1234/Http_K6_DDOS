// Package azcli is a thin wrapper around the `az` CLI for tasks where the SDK
// would be either heavier (Bicep deploy) or require extra wiring (account context).
//
// The wrapper exists so the controller has a single, mockable surface for
// subprocess-style Azure operations.
package azcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner shells out to `az`. Override Path / Env in tests.
type Runner struct {
	Path string
	Env  []string
}

// New returns a Runner that uses the `az` binary on PATH.
func New() *Runner { return &Runner{Path: "az"} }

// Run executes `az <args...>` and returns stdout. Any non-zero exit returns
// a wrapped error containing stderr.
func (r *Runner) Run(ctx context.Context, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, r.Path, args...)
	cmd.WaitDelay = 5 * time.Second
	if r.Env != nil {
		cmd.Env = r.Env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return stdout.Bytes(), fmt.Errorf("az %s: %w", argSummary(args), ctx.Err())
		}
		stderrStr := strings.TrimSpace(stderr.String())
		if stderrStr == "" {
			return stdout.Bytes(), fmt.Errorf("az %s: %w", argSummary(args), err)
		}
		return stdout.Bytes(), fmt.Errorf("az %s: %s: %w", argSummary(args), stderrStr, err)
	}
	return stdout.Bytes(), nil
}

// RunJSON runs az and decodes stdout as JSON into out.
func (r *Runner) RunJSON(ctx context.Context, out any, args ...string) error {
	b, err := r.Run(ctx, args...)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return errors.New("az produced empty JSON output")
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("decode az JSON: %w", err)
	}
	return nil
}

// AccountShow returns the current `az account show` summary.
type AccountShow struct {
	ID       string `json:"id"`
	TenantID string `json:"tenantId"`
	Name     string `json:"name"`
	User     struct {
		Name string `json:"name"`
	} `json:"user"`
}

// CurrentAccount returns the active subscription context.
func (r *Runner) CurrentAccount(ctx context.Context) (*AccountShow, error) {
	var a AccountShow
	if err := r.RunJSON(ctx, &a, "account", "show", "--output", "json"); err != nil {
		return nil, fmt.Errorf("az account show: %w (run `az login`?)", err)
	}
	return &a, nil
}

// SetSubscription points `az` at the given subscription ID.
func (r *Runner) SetSubscription(ctx context.Context, subID string) error {
	_, err := r.Run(ctx, "account", "set", "--subscription", subID)
	return err
}

// CreateResourceGroup creates a resource group if it doesn't already exist.
func (r *Runner) CreateResourceGroup(ctx context.Context, name, location string) error {
	_, err := r.Run(ctx,
		"group", "create",
		"--name", name,
		"--location", location,
		"--output", "none",
	)
	return err
}

// DeleteResourceGroup deletes a resource group asynchronously.
// Returns immediately; the deletion continues in Azure.
func (r *Runner) DeleteResourceGroup(ctx context.Context, name string) error {
	_, err := r.Run(ctx,
		"group", "delete",
		"--name", name,
		"--yes",
		"--no-wait",
		"--output", "none",
	)
	return err
}

// (no shared-key helpers — controller uses --auth-mode login throughout)

// BlobUpload writes data to a blob using the operator's az login OAuth token
// (--auth-mode login). Requires the operator to have Storage Blob Data
// Contributor (or higher) on the storage account.
func (r *Runner) BlobUpload(ctx context.Context, accountName, container, blobName string, data []byte) error {
	tmp, err := os.CreateTemp("", "ddos-blob-*.tmp")
	if err != nil {
		return fmt.Errorf("blob upload: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("blob upload: write temp file: %w", err)
	}
	tmp.Close()

	_, err = r.Run(ctx,
		"storage", "blob", "upload",
		"--account-name", accountName,
		"--auth-mode", "login",
		"--container-name", container,
		"--name", blobName,
		"--file", tmpPath,
		"--overwrite",
		"--output", "none",
	)
	return err
}

// GetSignedInUserObjectID returns the object ID of the currently signed-in
// Azure AD user (`az ad signed-in-user show`). Used to grant data-plane roles
// on resources the operator creates.
func (r *Runner) GetSignedInUserObjectID(ctx context.Context) (string, error) {
	var u struct {
		ID string `json:"id"`
	}
	if err := r.RunJSON(ctx, &u,
		"ad", "signed-in-user", "show",
		"--query", "{id:id}",
		"--output", "json",
	); err != nil {
		return "", fmt.Errorf("signed-in-user show: %w", err)
	}
	if u.ID == "" {
		return "", fmt.Errorf("signed-in-user show: empty id")
	}
	return u.ID, nil
}

// VMSSScale sets the capacity of a VMSS.
func (r *Runner) VMSSScale(ctx context.Context, rg, vmssName string, capacity int) error {
	_, err := r.Run(ctx,
		"vmss", "scale",
		"--resource-group", rg,
		"--name", vmssName,
		"--new-capacity", fmt.Sprintf("%d", capacity),
		"--output", "none",
	)
	return err
}

// VMSSInstanceInfo is a minimal view of a VMSS instance.
type VMSSInstanceInfo struct {
	InstanceID        string `json:"instanceId"`
	ProvisioningState string `json:"provisioningState"`
}

// VMSSListInstances returns all instances in a VMSS.
func (r *Runner) VMSSListInstances(ctx context.Context, rg, vmssName string) ([]VMSSInstanceInfo, error) {
	var instances []VMSSInstanceInfo
	if err := r.RunJSON(ctx, &instances,
		"vmss", "list-instances",
		"--resource-group", rg,
		"--name", vmssName,
		"--output", "json",
	); err != nil {
		return nil, fmt.Errorf("vmss list-instances: %w", err)
	}
	return instances, nil
}

// argSummary trims long Bicep file paths in error messages.
func argSummary(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if len(args) > 6 {
		return strings.Join(args[:6], " ") + " ..."
	}
	return strings.Join(args, " ")
}
