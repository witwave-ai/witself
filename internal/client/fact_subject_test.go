package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFactSubjectClient(t *testing.T) {
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer agent-token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/fact-subjects/spouse":
			seen["upsert"]++
			var in map[string]any
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decode upsert: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if in["display_name"] != "My spouse" || in["canonical_key"] != nil {
				t.Errorf("upsert input = %#v", in)
			}
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","subject":{"id":"sub_1","canonical_key":"spouse","display_name":"My spouse","aliases":[],"created_at":"2026-07-12T19:00:00Z","updated_at":"2026-07-12T19:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/fact-subjects/spouse/aliases":
			seen["alias"]++
			var in AddFactSubjectAliasInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decode alias: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if in.Alias != "my wife" || in.CanonicalKey != "" {
				t.Errorf("alias input = %#v", in)
			}
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","subject":{"id":"sub_1","canonical_key":"spouse","display_name":"My spouse","aliases":["my wife"],"created_at":"2026-07-12T19:00:00Z","updated_at":"2026-07-12T19:05:00Z"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/fact-subjects":
			seen["list"]++
			_, _ = w.Write([]byte(`{"schema_version":"witself.v0","subjects":[{"id":"sub_1","canonical_key":"spouse","display_name":"My spouse","aliases":["my wife"],"created_at":"2026-07-12T19:00:00Z","updated_at":"2026-07-12T19:05:00Z"}]}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	subject, err := UpsertFactSubject(context.Background(), srv.URL+"/", "agent-token", UpsertFactSubjectInput{
		CanonicalKey: "spouse",
		DisplayName:  "My spouse",
	})
	if err != nil {
		t.Fatal(err)
	}
	if subject.ID != "sub_1" || subject.CanonicalKey != "spouse" || subject.DisplayName != "My spouse" {
		t.Fatalf("upsert response = %#v", subject)
	}

	subject, err = AddFactSubjectAlias(context.Background(), srv.URL, "agent-token", AddFactSubjectAliasInput{
		CanonicalKey: "spouse",
		Alias:        "my wife",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(subject.Aliases) != 1 || subject.Aliases[0] != "my wife" {
		t.Fatalf("alias response = %#v", subject)
	}

	subjects, err := ListFactSubjects(context.Background(), srv.URL, "agent-token")
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].CanonicalKey != "spouse" {
		t.Fatalf("list response = %#v", subjects)
	}

	for _, operation := range []string{"upsert", "alias", "list"} {
		if seen[operation] != 1 {
			t.Errorf("%s requests = %d, want 1", operation, seen[operation])
		}
	}
}
