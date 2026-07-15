package main

import (
	"embed"
	"net/http"
)

//go:embed docs/openapi.yaml
var openAPISpec embed.FS

// scalarDocsHTML renders the Scalar API reference (https://scalar.com) via
// its CDN script — no Go client library exists for it, and none is needed:
// the whole "integration" is a static page pointed at the spec URL, which
// works the same from any backend language.
const scalarDocsHTML = `<!doctype html>
<html>
  <head>
    <title>groundcontrol API reference</title>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
  </head>
  <body>
    <script id="api-reference" data-url="/openapi.yaml"></script>
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
  </body>
</html>
`

func serveOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	b, err := openAPISpec.ReadFile("docs/openapi.yaml")
	if err != nil {
		http.Error(w, "spec not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Write(b)
}

func serveScalarDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(scalarDocsHTML))
}
