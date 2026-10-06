// Package cloudinit renders the cloud-init #cloud-config bootstrap document
// for DDOS worker VMs. The template + supporting shell/systemd files live in
// the repo-root cloud-init/ directory and are embedded into the binary so the
// CLI is self-contained.
package cloudinit

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io/fs"
	"net/url"
	"strings"
	"text/template"

	rootassets "github.com/patenvy1234/Http_K6_DDOS"
)

// Input collects every value the worker.yaml.tmpl template references.
// Values are derived by the orchestrator after the foundation Bicep deployment
// (which creates the UAMI + storage account) and before the fleet Bicep
// deployment (which consumes the rendered customData).
type Input struct {
	// Run identity
	RunID   string
	RunName string

	// SSH access
	AdminUsername string
	SSHPublicKey  string

	// Metrics ingestion (Azure Monitor managed Prometheus)
	DCEURL         string // e.g. https://ddos-dce-<region>-xxx.<region>-1.handler.control.monitor.azure.com
	DCRImmutableID string // 'dcr-xxxxxxxx' (NOT the ARM resource ID)
	DCRStreamName  string // typically "Microsoft-PrometheusMetrics"

	// Bundle delivery (profile.json + runner.js + lib/*)
	StorageAccount string
	BlobContainer  string
	BundleBlob     string // e.g. "bundle.tar.gz"

	// k6 binary version pin
	K6Version string
}

// K6RemoteWriteURL constructs the full DCE ingestion endpoint that k6's
// prometheus-rw output posts to. Built from DCEURL + DCRImmutableID + stream.
func (i Input) K6RemoteWriteURL() (string, error) {
	if i.DCEURL == "" || i.DCRImmutableID == "" {
		return "", fmt.Errorf("DCEURL and DCRImmutableID must be set")
	}
	stream := i.DCRStreamName
	if stream == "" {
		stream = "Microsoft-PrometheusMetrics"
	}
	base := strings.TrimRight(i.DCEURL, "/")
	return fmt.Sprintf(
		"%s/dataCollectionRules/%s/streams/%s/api/v1/write?api-version=2023-04-24",
		base, url.PathEscape(i.DCRImmutableID), url.PathEscape(stream),
	), nil
}

// Validate returns an error for any missing required field. Called at the
// top of Render to surface configuration mistakes early.
func (i Input) Validate() error {
	miss := func(name, val string) error {
		if strings.TrimSpace(val) == "" {
			return fmt.Errorf("cloudinit: %s is required", name)
		}
		return nil
	}
	for _, c := range []struct{ name, v string }{
		{"RunID", i.RunID},
		{"RunName", i.RunName},
		{"AdminUsername", i.AdminUsername},
		{"SSHPublicKey", i.SSHPublicKey},
		{"DCEURL", i.DCEURL},
		{"DCRImmutableID", i.DCRImmutableID},
		{"StorageAccount", i.StorageAccount},
		{"BlobContainer", i.BlobContainer},
		{"BundleBlob", i.BundleBlob},
		{"K6Version", i.K6Version},
	} {
		if err := miss(c.name, c.v); err != nil {
			return err
		}
	}
	return nil
}

// Render evaluates worker.yaml.tmpl with the provided input and returns the
// full #cloud-config document as bytes. Use Base64 for the VMSS customData
// property.
func Render(in Input) ([]byte, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	sub, err := fs.Sub(rootassets.CloudInitFS, "cloud-init")
	if err != nil {
		return nil, fmt.Errorf("cloudinit: sub fs: %w", err)
	}

	tmplBytes, err := fs.ReadFile(sub, "worker.yaml.tmpl")
	if err != nil {
		return nil, fmt.Errorf("cloudinit: read template: %w", err)
	}
	// cloud-init parses YAML in LF mode; strip CRs that may sneak in via a
	// Windows checkout so the rendered document is always LF-only.
	tmplBytes = []byte(strings.ReplaceAll(string(tmplBytes), "\r\n", "\n"))

	rwURL, err := in.K6RemoteWriteURL()
	if err != nil {
		return nil, err
	}

	funcs := template.FuncMap{
		// asset returns the verbatim text of an embedded file. The caller is
		// expected to pipe through `indent` to align it under the surrounding
		// YAML block.
		"asset": func(path string) (string, error) {
			b, err := fs.ReadFile(sub, path)
			if err != nil {
				return "", fmt.Errorf("cloudinit: read asset %s: %w", path, err)
			}
			// Normalize Windows line endings so YAML output is consistent
			// regardless of the dev's checkout.
			s := strings.ReplaceAll(string(b), "\r\n", "\n")
			return strings.TrimRight(s, "\n"), nil
		},

		// indent prefixes every line BUT the first with `spaces` spaces. The
		// surrounding template line already provides indentation for the first
		// line, so we only need to handle subsequent lines.
		"indent": func(spaces int, s string) string {
			pad := strings.Repeat(" ", spaces)
			return strings.ReplaceAll(s, "\n", "\n"+pad)
		},
	}

	tmpl, err := template.New("worker.yaml").
		Option("missingkey=error").
		Funcs(funcs).
		Parse(string(tmplBytes))
	if err != nil {
		return nil, fmt.Errorf("cloudinit: parse: %w", err)
	}

	// Layer RemoteWriteURL into the data so the template can reference it
	// without having to know it's a method call.
	data := struct {
		Input
		K6RemoteWriteURL string
	}{
		Input:            in,
		K6RemoteWriteURL: rwURL,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("cloudinit: execute: %w", err)
	}
	return buf.Bytes(), nil
}

// RenderBase64 calls Render and base64-encodes the result for use as the
// VMSS customData property (Azure expects base64-encoded #cloud-config).
func RenderBase64(in Input) (string, error) {
	b, err := Render(in)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
