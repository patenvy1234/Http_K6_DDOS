package config

import "regexp"

// runNameRegexp restricts runName to lowercase alphanumerics + dashes, 3–48 chars.
// runName is interpolated into resource names; Azure RG names allow more, but we stay
// conservative so the result is also valid for storage accounts, DNS, etc.
var runNameRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,46}[a-z0-9]$`)
