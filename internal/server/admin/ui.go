package admin

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed ui
var uiFiles embed.FS

// uiHandler serves the embedded single-page panel.
//
// Unknown paths fall back to index.html so the client-side router owns
// navigation, but /api/ is never rewritten: a mistyped API path must return a
// JSON 404, not an HTML page the caller cannot parse.
func (s *Server) uiHandler() http.Handler {
	sub, err := fs.Sub(uiFiles, "ui")
	if err != nil {
		// The files are compiled in, so this cannot fail at runtime.
		panic("admin ui assets missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/" {
			writeError(w, http.StatusNotFound, "no such endpoint")
			return
		}

		if r.URL.Path != "/" {
			if f, ferr := sub.Open(trimLeadingSlash(r.URL.Path)); ferr == nil {
				_ = f.Close()
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})
}

func trimLeadingSlash(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p[1:]
	}
	return p
}
