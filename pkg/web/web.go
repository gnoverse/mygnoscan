// Package web serves the single-file frontend embedded in the binary.
package web

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
)

//go:embed frontend
var frontendFS embed.FS

// Handler serves the frontend: a static file when the path names one, and
// index.html for everything else.
//
// The fallback is what makes deep links work. The frontend is a single-page app
// that routes on the URL, so /realm/gno.land/r/demo/boards has to arrive at the
// same HTML as / — serving a 404 there would break every link anyone shares.
//
// The static attempt comes first and is an existence check rather than a path
// prefix: anything actually embedded is served as itself, and only genuine
// misses fall through to the app.
func Handler() (http.HandlerFunc, error) {
	sub, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		return nil, fmt.Errorf("frontend fs: %w", err)
	}
	static := http.FileServer(http.FS(sub))
	index, err := fs.ReadFile(frontendFS, "frontend/index.html")
	if err != nil {
		return nil, fmt.Errorf("read index.html: %w", err)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			if f, err := sub.Open(r.URL.Path[1:]); err == nil {
				f.Close()
				static.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	}, nil
}

// Index is the embedded index.html, for callers that need the bytes rather than
// a handler.
func Index() ([]byte, error) { return fs.ReadFile(frontendFS, "frontend/index.html") }
