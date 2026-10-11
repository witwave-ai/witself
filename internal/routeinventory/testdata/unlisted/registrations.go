package fixture

import "net/http"

func unlisted(mux *http.ServeMux, h http.HandlerFunc) {
	mux.HandleFunc("GET /unlisted", h)
}
