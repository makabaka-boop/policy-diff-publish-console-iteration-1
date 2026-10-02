package main

import (
	"io/fs"
	"log"
	"net/http"
	"strings"

	"accesssim/api"
)

func main() {
	mux := http.NewServeMux()

	srv := api.NewDefaultServer()
	srv.Routes(mux)

	// Serve the embedded Vue build; fall back to index.html for the
	// single page app. In dev, run `npm run dev` for Vite hot reload.
	staticFS, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		log.Fatalf("embedded filesystem: %v", err)
	}
	mux.Handle("/", spaHandler{static: http.FileServer(http.FS(staticFS)), fsys: staticFS})

	addr := ":8080"
	log.Printf("access-policy SIMULATOR (not real authz) listening on http://localhost%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

type spaHandler struct {
	static http.Handler
	fsys   fs.FS
}

func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}
	if _, err := fs.Stat(h.fsys, p); err != nil {
		// Unknown path: let the SPA route it.
		r.URL.Path = "/"
	}
	h.static.ServeHTTP(w, r)
}
