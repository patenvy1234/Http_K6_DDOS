// Package runspec produces the per-run identity values consumed by the
// two-phase Bicep deploy (foundation.bicep + fleet.bicep) and the cloud-init
// template.
//
// It is intentionally kept SDK-free so unit tests do not need Azure credentials.
package runspec

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/patenvy1234/Http_K6_DDOS/internal/config"
)

// RunSpec carries every value derived for a single load run.
//
// The Go controller computes this once at the top of `ddos run` and feeds the
// values into both:
//   • the Bicep deployments (foundation.bicep + fleet.bicep parameters,
//     assembled in internal/orchestrator/runner)
//   • the cloud-init template (profile.json embedded in customData)
type RunSpec struct {
	RunName          string
	RunSuffix        string // 8-char unique suffix
	ResourceGroup    string
	AutoDeleteAt     time.Time
	Owner            string
	SSHPrivateKeyPEM string // operator-side, never sent to Azure
	SSHPublicKeyOpen string // sent to VMSS as authorized_keys
	StartAt          time.Time
}

// New derives a fresh RunSpec from a validated profile.
//
// The `owner` string is recorded as a tag on every Azure resource so operators
// can attribute orphaned spend back to whoever launched the run.
func New(profile *config.AttackProfile, owner string) (*RunSpec, error) {
	if profile == nil {
		return nil, fmt.Errorf("profile must not be nil")
	}
	suffix := shortID()
	rg := fmt.Sprintf("ddos-%s-%s", profile.RunName, suffix)

	priv, pub, err := generateSSHKeypair()
	if err != nil {
		return nil, fmt.Errorf("generate ssh keypair: %w", err)
	}

	now := time.Now().UTC()
	ttl := time.Duration(profile.Lifecycle.TTLMinutes) * time.Minute

	return &RunSpec{
		RunName:          profile.RunName,
		RunSuffix:        suffix,
		ResourceGroup:    rg,
		AutoDeleteAt:     now.Add(ttl),
		Owner:            owner,
		SSHPrivateKeyPEM: priv,
		SSHPublicKeyOpen: pub,
		StartAt:          now.Add(5 * time.Minute),
	}, nil
}

// shortID returns an 8-character lowercase hex suffix derived from a fresh UUID.
func shortID() string {
	u := uuid.New()
	return fmt.Sprintf("%x", u[:4])
}

func generateSSHKeypair() (privPEM string, pubOpenSSH string, err error) {
	priv, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return "", "", err
	}
	privBytes := x509.MarshalPKCS1PrivateKey(priv)
	privPEM = string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: privBytes,
	}))
	sshPub, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		return "", "", err
	}
	pubOpenSSH = string(ssh.MarshalAuthorizedKey(sshPub))
	return privPEM, pubOpenSSH, nil
}

// dcrNameFromResourceID extracts the DCR short name from a full ARM resource ID.
// Defensive: returns the input unchanged if the format is unexpected.
func dcrNameFromResourceID(id string) string {
	if id == "" {
		return ""
	}
	if id[len(id)-1] == '/' {
		id = id[:len(id)-1]
	}
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '/' {
			return id[i+1:]
		}
	}
	return id
}
