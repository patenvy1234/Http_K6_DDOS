package ddos

import "embed"

// InfraFS embeds the infra/grafana dashboard JSON files and the safety-net
// Azure Function code so the CLI ships as a single self-contained binary.
//
//go:embed infra/grafana infra/function
var InfraFS embed.FS
