// Package docs embeds the public v1 API description in the orchestrator.
package docs

import _ "embed"

//go:embed openapi.yaml
var Contract []byte
