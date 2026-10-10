package fixture

import "net/http"

func first(mux *http.ServeMux, h http.HandlerFunc) {
	mux.HandleFunc("GET /duplicate", h)
}

func second(mux *http.ServeMux, h http.HandlerFunc) {
	mux.HandleFunc("GET /duplicate", h)
}
