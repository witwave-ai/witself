package fixture

import "net/http"

var global = func() int {
	http.HandleFunc("GET /global", nil)
	return 0
}()
