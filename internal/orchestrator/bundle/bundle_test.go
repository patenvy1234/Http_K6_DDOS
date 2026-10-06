package bundle

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func validProfile() []byte {
	p := map[string]any{
		"run_id":             "ab12cd34",
		"run_name":           "smoke",
		"start_at":           "2026-05-24T22:40:00Z",
		"target_rps":         1000,
		"ramp_up_seconds":    30,
		"duration_seconds":   60,
		"ramp_down_seconds":  30,
		"pattern":            "keepalive-burst",
		"endpoints": []map[string]any{
			{"id": "ping", "url": "https://example.com/ping", "method": "GET", "weight": 100},
		},
	}
	b, _ := json.Marshal(p)
	return b
}

func TestBuildContainsExpectedFiles(t *testing.T) {
	data, err := Build(validProfile())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	files := tarEntries(t, data)

	mustHave := []string{
		"runner.js",
		"lib/profile.js",
		"lib/transport.js",
		"lib/metrics.js",
		"scenarios/keepalive-burst.js",
		"scenarios/new-conn-flood.js",
		"profile.json",
	}
	for _, f := range mustHave {
		if _, ok := files[f]; !ok {
			t.Errorf("bundle missing: %s (entries: %v)", f, keys(files))
		}
	}
}

func TestBuildRejectsInvalidProfileJSON(t *testing.T) {
	if _, err := Build([]byte("not-json")); err == nil {
		t.Error("expected error for invalid profile JSON")
	}
}

func TestBuildProfileJSONIsPrettyPrinted(t *testing.T) {
	data, err := Build(validProfile())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	files := tarEntries(t, data)
	profileContent, ok := files["profile.json"]
	if !ok {
		t.Fatal("profile.json not in bundle")
	}
	if !strings.Contains(profileContent, "\n  ") {
		t.Error("profile.json not pretty-printed")
	}
}

func TestBuildK6ScriptsHaveNoCarriageReturns(t *testing.T) {
	data, err := Build(validProfile())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for name, content := range tarEntries(t, data) {
		if strings.HasSuffix(name, ".js") && strings.Contains(content, "\r") {
			t.Errorf("%s contains CR (\\r) characters", name)
		}
	}
}

// tarEntries decompresses + untars data and returns a map of filename → content.
func tarEntries(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := make(map[string]string)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar read %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = string(b)
	}
	return out
}

func keys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
