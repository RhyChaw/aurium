package api

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web
var webFS embed.FS

// dashboard serves the embedded dashboard (§11.3).
//
// It is embedded rather than served from disk so the daemon is a single
// binary with nothing to install, and so a partial checkout cannot leave a
// half-working UI.
func (s *Server) dashboard() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusInternalServerError, "dashboard assets missing")
		})
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The dashboard is a single page; unknown paths fall back to it so a
		// refresh on a sub-route does not 404.
		if r.URL.Path != "/" && !fileExists(sub, r.URL.Path[1:]) {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		if r.URL.Path == "/" {
			s.serveIndex(w, sub)
			return
		}
		files.ServeHTTP(w, r)
	})
}

// serveIndex injects the host token into the page so the dashboard can call
// the same API the CLI does.
//
// The daemon listens on loopback only, so anything that can fetch this page
// could already read ~/.aurium/token. Injecting it server-side keeps it out of
// the URL bar, out of browser history and out of screenshots, which a
// #token=... fragment would not.
func (s *Server) serveIndex(w http.ResponseWriter, sub fs.FS) {
	body, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "dashboard index missing")
		return
	}
	tag := fmt.Sprintf(`<meta name="aurium-token" content=%q>`, s.HostToken)
	page := strings.Replace(string(body), "<title>", tag+"\n<title>", 1)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The token is in the body, so this page must never be cached to disk by
	// an intermediary or the browser.
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(page))
}

func fileExists(fsys fs.FS, name string) bool {
	if name == "" {
		return false
	}
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	f.Close()
	return true
}
