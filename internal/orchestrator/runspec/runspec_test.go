package runspec

import (
	"strings"
	"testing"

	"github.com/patenvy1234/Http_K6_DDOS/internal/config"
)

func TestNewRunSpecDerivedFields(t *testing.T) {
	p := &config.AttackProfile{
		RunName: "smoke-test",
		Lifecycle: config.Lifecycle{TTLMinutes: 30},
		Fleet:     config.Fleet{Regions: []string{"eastus"}, VMSize: "Standard_F4s_v2", InitialVMCount: 1, MaxVMCount: 2, UseSpot: true},
	}

	spec, err := New(p, "tester@example.com")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(spec.RunSuffix) != 8 {
		t.Errorf("RunSuffix length: got %d, want 8", len(spec.RunSuffix))
	}
	if !strings.HasPrefix(spec.ResourceGroup, "ddos-smoke-test-") {
		t.Errorf("ResourceGroup prefix: got %q", spec.ResourceGroup)
	}
	if !strings.Contains(spec.SSHPrivateKeyPEM, "BEGIN RSA PRIVATE KEY") {
		t.Errorf("SSHPrivateKeyPEM: missing PEM header")
	}
	if !strings.HasPrefix(spec.SSHPublicKeyOpen, "ssh-rsa ") {
		t.Errorf("SSHPublicKeyOpen: not OpenSSH format: %q", spec.SSHPublicKeyOpen)
	}
	if spec.AutoDeleteAt.Before(spec.StartAt) {
		t.Errorf("AutoDeleteAt %v before StartAt %v", spec.AutoDeleteAt, spec.StartAt)
	}
}

func TestDCRNameFromResourceID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"/subscriptions/x/resourceGroups/y/providers/Microsoft.Insights/dataCollectionRules/my-dcr", "my-dcr"},
		{"/subscriptions/x/.../my-dcr/", "my-dcr"},
		{"just-a-name", "just-a-name"},
	}
	for _, c := range cases {
		if got := dcrNameFromResourceID(c.in); got != c.want {
			t.Errorf("dcrNameFromResourceID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
