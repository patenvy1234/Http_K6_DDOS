// Package monitor provides a thin client for querying the Azure Monitor Workspace
// (managed Prometheus) compatible query endpoint.
//
// Auth uses an Entra Bearer token scoped to https://prometheus.monitor.azure.com
// obtained via `az account get-access-token`.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/patenvy1234/Http_K6_DDOS/internal/azure/azcli"
)

// Client queries the Azure Monitor Workspace Prometheus-compatible HTTP API.
type Client struct {
	queryURL string // e.g. https://ddos-amw-xxx.eastus.prometheus.monitor.azure.com
	az       *azcli.Runner
	hc       *http.Client
}

// New creates a Client from the Prometheus query base URL stored in userconfig.
// queryURL should end without a trailing slash.
func New(queryURL string, az *azcli.Runner) *Client {
	return &Client{
		queryURL: strings.TrimRight(queryURL, "/"),
		az:       az,
		hc:       &http.Client{Timeout: 15 * time.Second},
	}
}

// promResponse is the Prometheus HTTP API envelope.
type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]json.RawMessage `json:"value"` // [timestamp, value]
		} `json:"result"`
	} `json:"data"`
	Error     string `json:"error,omitempty"`
	ErrorType string `json:"errorType,omitempty"`
}

// QueryScalar executes a PromQL instant query and returns the first scalar value.
// Returns 0 and no error when the result set is empty (metric not yet written).
func (c *Client) QueryScalar(ctx context.Context, promQL string) (float64, error) {
	token, err := c.token(ctx)
	if err != nil {
		return 0, fmt.Errorf("monitor: token: %w", err)
	}

	endpoint := c.queryURL + "/api/v1/query"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(
		"query="+url.QueryEscape(promQL),
	))
	if err != nil {
		return 0, fmt.Errorf("monitor: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("monitor: query request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("monitor: query HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return 0, fmt.Errorf("monitor: parse response: %w", err)
	}
	if pr.Status != "success" {
		return 0, fmt.Errorf("monitor: query error (%s): %s", pr.ErrorType, pr.Error)
	}
	if len(pr.Data.Result) == 0 {
		return 0, nil // no data yet
	}
	// result[0].Value = [timestamp, "value_string"]
	var valStr string
	if err := json.Unmarshal(pr.Data.Result[0].Value[1], &valStr); err != nil {
		return 0, fmt.Errorf("monitor: parse value: %w", err)
	}
	var val float64
	if _, err := fmt.Sscanf(valStr, "%f", &val); err != nil {
		return 0, fmt.Errorf("monitor: parse float %q: %w", valStr, err)
	}
	return val, nil
}

// rpsWindow is the PromQL range vector used for rate() queries. It must be
// wider than the worst-case DCE remote-write latency (observed ~40 s) so that
// samples which arrive late still fall inside the window. 3 m gives a 4–5×
// safety margin without making the scaler unacceptably slow to react.
const rpsWindow = "3m"

// ObservedRPS returns the sum of actual HTTP requests/s across all workers for
// a given runID, measured over the past rpsWindow.
func (c *Client) ObservedRPS(ctx context.Context, runID string) (float64, error) {
	q := fmt.Sprintf(`sum(rate(k6_http_reqs_total{runid=%q}[%s]))`, runID, rpsWindow)
	return c.QueryScalar(ctx, q)
}

// ObservedDroppedRate returns iterations/s that k6 dropped because its
// arrival-rate scheduler could not allocate a free VU in time. A non-zero value
// means at least one worker is saturated (CPU, conntrack, ephemeral ports, or
// VU pool exhausted), regardless of whether aggregate RPS is at target.
func (c *Client) ObservedDroppedRate(ctx context.Context, runID string) (float64, error) {
	q := fmt.Sprintf(`sum(rate(k6_dropped_iterations_total{runid=%q}[%s]))`, runID, rpsWindow)
	return c.QueryScalar(ctx, q)
}

// token fetches an Entra Bearer token for the AMW Prometheus resource.
func (c *Client) token(ctx context.Context) (string, error) {
	var out struct {
		AccessToken string `json:"accessToken"`
	}
	if err := c.az.RunJSON(ctx, &out,
		"account", "get-access-token",
		"--resource", "https://prometheus.monitor.azure.com",
		"--output", "json",
	); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("empty access token returned")
	}
	return out.AccessToken, nil
}
