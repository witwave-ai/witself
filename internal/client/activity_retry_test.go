package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/witwave-ai/witself/internal/activity"
)

func TestActivityRetryScopeDistinguishesRequests(t *testing.T) {
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = append(ids, r.Header.Get(activity.RequestIDHeader))
		if !activity.ValidRequestID(ids[len(ids)-1]) {
			t.Error("missing activity ID")
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	ctx := WithActivityRetryScope(context.Background())
	for _, call := range []struct {
		ctx        context.Context
		path, body string
	}{
		{ctx, "/v1/memories:recall", `{"query":"one"}`},
		{WithActivityRetryScope(ctx), "/v1/memories:recall", `{"query":"one"}`},
		{ctx, "/v1/memories:recall", `{"query":"two"}`},
		{ctx, "/v1/memories:recall?cursor=page2", `{"query":"one"}`},
		{WithActivityRetryScope(context.Background()), "/v1/memories:recall", `{"query":"one"}`},
	} {
		if err := doJSON(call.ctx, http.MethodPost, srv.URL+call.path, "synthetic", []byte(call.body), nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(ids) != 5 || ids[0] != ids[1] {
		t.Fatal("exact retry did not retain intent")
	}
	for i := 2; i < len(ids); i++ {
		for j := 0; j < i; j++ {
			if ids[i] == ids[j] {
				t.Fatal("different request reused intent")
			}
		}
	}
}
