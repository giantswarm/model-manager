// Package api publishes the model-manager API contract. The OpenAPI document
// is the source of truth sibling components (portal backend, umbrella chart,
// agentlab) build against; the server serves it at /api/v1/openapi.yaml.
package api

import _ "embed"

// OpenAPI is the REST contract.
//
//go:embed openapi.yaml
var OpenAPI []byte
