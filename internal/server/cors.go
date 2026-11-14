package server

import "net/http"

// corsReadable lists the read-only endpoints other origins may call, e.g. a
// start page opened as a local file. They get "Access-Control-Allow-Origin: *"
// but never credentials, so a foreign page cannot ride on the browser's
// session cookie: it needs the API token as "Authorization: Bearer <token>".
// The CSRF header is not among the allowed headers, so nothing that changes
// state becomes reachable cross-origin.
var corsReadable = map[string]bool{
	"/api/notifications": true,
}

func allowReadOnlyCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !corsReadable[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Headers", "Authorization")
		h.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		h.Set("Access-Control-Max-Age", "86400")
		switch r.Method {
		case http.MethodOptions:
			// Chromium asks before a page reaches a local address.
			h.Set("Access-Control-Allow-Private-Network", "true")
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet, http.MethodHead:
			next.ServeHTTP(w, r)
		default:
			// Writes to these paths stay same-origin only.
			h.Del("Access-Control-Allow-Origin")
			next.ServeHTTP(w, r)
		}
	})
}
