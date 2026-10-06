// Package ddos — k6 script assets embedded at compile time.
// Separate from CloudInitFS so the embed path stays tidy.
//
// This file is in the root package (package ddos) so the //go:embed directive
// can reach the k6/ directory next to go.mod.

package ddos

import "embed"

// K6FS embeds the entire k6/ directory:
//   - runner.js                (main entry point, deployed to /etc/ddos/runner.js)
//   - lib/{profile,transport,metrics}.js
//   - scenarios/{keepalive-burst,new-conn-flood}.js
//
//go:embed k6
var K6FS embed.FS
