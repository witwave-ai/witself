package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestViewerContextImmutableBinding(t *testing.T) {
	f, cfg := newAccountFixture(t)
	collector := newAccountCollector(cfg)
	handler := viewerContextHandler(cfg, collector)
	binding := ViewerBinding{AccountID: cfg.AccountManager.Identity.AccountID, OperatorID: cfg.AccountManager.Identity.OperatorID, Role: cfg.AccountManager.Identity.Role}
	// Prove the handler captured the original value, not an externally mutable pointer.
	cfg.AccountManager.Identity.OperatorID = "op_mutated"
	for _, revoked := range []bool{false, true} {
		f.change(func() { f.revoked = revoked })
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/console/viewer", nil))
		var got ViewerContext
		if json.Unmarshal(w.Body.Bytes(), &got) != nil || got.SchemaVersion != ViewerSchema || got.Manager == nil || *got.Manager != binding || got.Available == revoked {
			t.Fatal("viewer lost immutable binding or availability")
		}
		if strings.Contains(w.Body.String(), "fake-manager") || strings.Contains(w.Body.String(), cfg.Endpoint) {
			t.Fatal("viewer leaked private configuration")
		}
	}
}

func TestViewerContextUsesConsoleGuards(t *testing.T) {
	_, cfg := newAccountFixture(t)
	cfg.AccessToken = "synthetic-access"
	mux := http.NewServeMux()
	if err := Register(mux, cfg); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "http://127.0.0.1:54321/api/console/viewer", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, request)
	if w.Code != 401 {
		t.Fatal("viewer without local authentication")
	}
	exchange := httptest.NewRequest("GET", "http://127.0.0.1:54321/?token=synthetic-access", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, exchange)
	cookies := w.Result().Cookies()
	if w.Code != 303 || len(cookies) != 1 {
		t.Fatal("exchange failed")
	}
	request.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, request)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("viewer security wrappers missing")
	}
}
