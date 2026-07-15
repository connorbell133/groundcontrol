// Package groundcontrol exists only to hold the go:embed directives: embed
// paths cannot reach a parent directory, so the PWA in public/ and the OpenAPI
// spec in docs/ must be embedded from the module root where they live.
package groundcontrol

import "embed"

//go:embed all:public
var PublicFS embed.FS

//go:embed docs/openapi.yaml
var OpenAPIFS embed.FS
