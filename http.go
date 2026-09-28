package propshaft

import (
	"bytes"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// Handler serves only known fingerprinted paths beneath Config.Prefix. Mount it
// without http.StripPrefix: it expects the original request path. It supports
// GET, HEAD, ETags, conditional requests, and byte ranges. There are no directory
// listings, logical-path redirects, or fallbacks for obsolete fingerprints.
func (p *Pipeline) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		name, ok := strings.CutPrefix(r.URL.Path, p.prefix.mount)
		if !ok || !validName(name) {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		snap, err := p.current()
		if err != nil {
			// Development errors are intentionally visible locally. Production
			// snapshots cannot fail here or expose filesystem diagnostics.
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		asset, ok := snap.output[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if !p.development {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		w.Header().Set("ETag", `"`+asset.digest+`"`)
		if contentType := mime.TypeByExtension(path.Ext(name)); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		http.ServeContent(cacheResponseWriter{w}, r, name, time.Time{}, bytes.NewReader(asset.data))
	})
}

// ServeContent can emit precondition and range errors after immutable caching
// has been set. Only successful representations may retain that cache policy.
type cacheResponseWriter struct{ http.ResponseWriter }

func (w cacheResponseWriter) WriteHeader(status int) {
	if status >= 400 {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.ResponseWriter.WriteHeader(status)
}
