// Package ddos exposes top-level assets that need to live next to go.mod so
// the //go:embed directive can reach them (embed cannot ascend out of its
// declaring package directory).
package ddos

import "embed"

// CloudInitFS embeds the entire cloud-init/ tree:
//   - worker.yaml.tmpl       (the Go template rendered per run)
//   - sysctl/, limits/       (drop-in tuning files)
//   - scripts/               (token refresh, blob fetch, k6 install, etc.)
//   - units/                 (systemd service files)
//
//go:embed cloud-init
var CloudInitFS embed.FS
