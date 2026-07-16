package api

import (
	"net/http"

	groundcontrol "github.com/connorbell133/groundcontrol"
)

// scalarDocsHTML renders the Scalar API reference (https://scalar.com) via
// its CDN script — no Go client library exists for it, and none is needed:
// the whole "integration" is a static page pointed at the spec URL, which
// works the same from any backend language. The script is pinned to major v1
// so a future breaking release can't silently blank the page, and the static
// fallback below the mount point keeps /docs useful (a link to the raw spec)
// when the CDN is unreachable — e.g. a tailnet-only box — or JS is off; Scalar
// replaces the mount's contents once it boots.
const scalarDocsHTML = `<!doctype html>
<html>
  <head>
    <title>groundcontrol API reference</title>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
  </head>
  <body>
    <script id="api-reference" data-url="/openapi.yaml"></script>
    <div id="cdn-fallback" hidden style="font-family: system-ui, sans-serif; margin: 2rem">
      <p>The interactive reference could not load (cdn.jsdelivr.net is
      unreachable from this browser).</p>
      <p>The machine-readable contract is always served locally at
      <a href="/openapi.yaml">/openapi.yaml</a>, and the prose guide lives in
      the repo at docs/api.md.</p>
    </div>
    <noscript>
      <p style="font-family: system-ui, sans-serif; margin: 2rem">
        This interactive reference needs JavaScript. The raw spec is at
        <a href="/openapi.yaml">/openapi.yaml</a>.
      </p>
    </noscript>
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@1"
      onerror="document.getElementById('cdn-fallback').hidden = false"></script>
  </body>
</html>
`

func ServeOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	b, err := groundcontrol.OpenAPIFS.ReadFile("docs/openapi.yaml")
	if err != nil {
		http.Error(w, "spec not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Write(b)
}

func ServeScalarDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(scalarDocsHTML))
}
