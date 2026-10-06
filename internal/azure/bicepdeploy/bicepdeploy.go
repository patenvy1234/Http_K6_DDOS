// Package bicepdeploy wraps `az deployment` commands for subscription-scoped and
// resource-group-scoped deployments. We use az + Bicep (rather than the ARM SDK)
// because az handles Bicep compilation transparently and surfaces the same
// what-if / preview behaviour we want operators to see interactively.
package bicepdeploy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/azcli"
)

// Outputs is the decoded `properties.outputs` map from a successful deployment.
// Values are kept as raw JSON because outputs can be strings, ints, or objects.
type Outputs map[string]json.RawMessage

// StringArray returns the string-slice value at key, or nil if absent.
func (o Outputs) StringArray(key string) []string {
	raw, ok := o[key]
	if !ok {
		return nil
	}
	var wrapper struct {
		Value []string `json:"value"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil
	}
	return wrapper.Value
}

// String returns the string value at key, or "" if absent / wrong type.
func (o Outputs) String(key string) string {
	raw, ok := o[key]
	if !ok {
		return ""
	}
	var wrapper struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return ""
	}
	return wrapper.Value
}

// SubscriptionScope deploys a Bicep file at subscription scope.
func SubscriptionScope(ctx context.Context, az *azcli.Runner, opts Options) (Outputs, error) {
	if opts.Location == "" {
		return nil, fmt.Errorf("subscription-scope deployment requires location")
	}
	paramArgs, cleanup, err := opts.parametersAsArgs()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	args := append([]string{
		"deployment", "sub", "create",
		"--name", opts.Name,
		"--location", opts.Location,
		"--template-file", opts.TemplateFile,
		"--output", "json",
	}, paramArgs...)
	return run(ctx, az, args)
}

// ResourceGroupScope deploys a Bicep file into an existing RG. The RG must
// exist (use az.Run to create it first if needed).
func ResourceGroupScope(ctx context.Context, az *azcli.Runner, rg string, opts Options) (Outputs, error) {
	if rg == "" {
		return nil, fmt.Errorf("RG-scope deployment requires resource group")
	}
	paramArgs, cleanup, err := opts.parametersAsArgs()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	args := append([]string{
		"deployment", "group", "create",
		"--resource-group", rg,
		"--name", opts.Name,
		"--template-file", opts.TemplateFile,
		"--output", "json",
	}, paramArgs...)
	return run(ctx, az, args)
}

// Options is the deployment payload.
type Options struct {
	Name         string
	Location     string // only for subscription-scope deployments
	TemplateFile string
	Parameters   map[string]any
}

// parametersAsArgs writes parameters to a temp ARM parameters JSON file and
// returns `--parameters @<path>`. A temp file is used unconditionally because
// inline `key=value` blows past Windows' 8191-char command-line limit as soon
// as we pass anything large (e.g. base64 cloud-init customData).
// The returned cleanup func deletes the temp file and must be called.
func (o Options) parametersAsArgs() ([]string, func(), error) {
	noop := func() {}
	if len(o.Parameters) == 0 {
		return nil, noop, nil
	}

	type paramValue struct {
		Value any `json:"value"`
	}
	wrapped := make(map[string]paramValue, len(o.Parameters))
	for k, v := range o.Parameters {
		wrapped[k] = paramValue{Value: v}
	}
	doc := map[string]any{
		"$schema":        "https://schema.management.azure.com/schemas/2019-04-01/deploymentParameters.json#",
		"contentVersion": "1.0.0.0",
		"parameters":     wrapped,
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, noop, fmt.Errorf("encode parameters: %w", err)
	}
	f, err := os.CreateTemp("", "ddos-params-*.json")
	if err != nil {
		return nil, noop, fmt.Errorf("create temp params file: %w", err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, noop, fmt.Errorf("write temp params file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return nil, noop, fmt.Errorf("close temp params file: %w", err)
	}
	path := f.Name()
	cleanup := func() { os.Remove(path) }
	return []string{"--parameters", "@" + path}, cleanup, nil
}

func run(ctx context.Context, az *azcli.Runner, args []string) (Outputs, error) {
	out, err := az.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Properties struct {
			ProvisioningState string  `json:"provisioningState"`
			Outputs           Outputs `json:"outputs"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		preview := strings.TrimSpace(string(out))
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		return nil, fmt.Errorf("decode deployment response: %w (got: %s)", err, preview)
	}
	if resp.Properties.ProvisioningState != "" && resp.Properties.ProvisioningState != "Succeeded" {
		return resp.Properties.Outputs, fmt.Errorf("deployment provisioningState=%s", resp.Properties.ProvisioningState)
	}
	return resp.Properties.Outputs, nil
}
