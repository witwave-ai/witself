package fixture

import "net/http"

// Both the unknown enclosing function and the dynamic pattern would be errors
// if ParseDir accidentally included this file.
func testOnly(mux *http.ServeMux, p string, h http.HandlerFunc) {
	mux.HandleFunc(p, h)
}
