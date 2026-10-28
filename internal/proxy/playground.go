package proxy

import (
	_ "embed"
	"html/template"
	"net/http"
)

//go:embed web/index.html
var playgroundHTML string

//go:embed web/style.css
var playgroundCSS string

//go:embed web/app.js
var playgroundJS string

//go:embed web/sse.mjs
var playgroundSSE string
var playgroundTemplate = template.Must(template.New("playground").Parse(playgroundHTML))

func (h *Handler) playground(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	models := []string{"demo-model"}
	protected := false
	if h.access != nil {
		models = h.access.models
		protected = len(h.access.tenants) > 0
	}
	if err := playgroundTemplate.Execute(w, map[string]any{"Provider": h.provider.Name(), "Models": models, "Protected": protected}); err != nil {
		h.logger.Debug("playground response interrupted")
	}
}

func staticAsset(content, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write([]byte(content))
	}
}
