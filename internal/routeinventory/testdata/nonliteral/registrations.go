package fixture

import "net/http"

func register(mux *http.ServeMux, p string, h http.HandlerFunc) {
	mux.HandleFunc(p, h)
}
