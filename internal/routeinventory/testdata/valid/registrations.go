package fixture

import "net/http"

func register(mux *http.ServeMux, h http.HandlerFunc, alternate bool) {
	mux.HandleFunc("GET /single", h)
	mux.HandleFunc(
		"POST /multi",
		h)
	mux.HandleFunc("/ready", h)
	if alternate {
		mux.HandleFunc("GET /duplicate", h)
	} else {
		mux.HandleFunc("GET /duplicate", h)
	}
	mux.Handle("PUT /handled", h)
	func() {
		mux.HandleFunc(`/nested`, h)
	}()
}
