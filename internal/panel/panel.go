// Package panel embeds the two pages, their scripts, and the vendored
// xterm.js build. No CDN, no fonts, no analytics, no service worker.
package panel

import (
	"embed"
	"net/http"
)

// Named files only: the vendor dir must never grow surprise embeds.
//
//go:embed index.html index.js term.html term.js
//go:embed vendor/xterm.js vendor/xterm.css vendor/addon-fit.js
var files embed.FS

// csp is the strict policy for everything we serve. style-src carries
// 'unsafe-inline' ONLY because xterm.js injects its layout stylesheet at
// runtime via a <style> element; nothing else inlines styles.
const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' ws: wss:; img-src 'self' data:"

func serve(name, ctype string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := files.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("Content-Type", ctype)
		w.Write(b)
	})
}

// Index is the machine-list panel at /.
func Index() http.Handler { return serve("index.html", "text/html; charset=utf-8") }

// Term is the terminal page at /term/{sid}.
func Term() http.Handler { return serve("term.html", "text/html; charset=utf-8") }

// Assets serves /index.js, /term.js and /vendor/* from the embed.
func Assets() http.Handler {
	fs := http.FileServerFS(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		fs.ServeHTTP(w, r)
	})
}
