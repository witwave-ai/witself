package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// FactSubject is one stable, agent-owned identity used by durable facts.
type FactSubject struct {
	ID           string    `json:"id"`
	CanonicalKey string    `json:"canonical_key"`
	DisplayName  string    `json:"display_name,omitempty"`
	Aliases      []string  `json:"aliases"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// UpsertFactSubjectInput creates or updates one canonical subject.
type UpsertFactSubjectInput struct {
	CanonicalKey string `json:"-"`
	DisplayName  string `json:"display_name,omitempty"`
}

// AddFactSubjectAliasInput attaches one conversational alias to a subject.
type AddFactSubjectAliasInput struct {
	CanonicalKey string `json:"-"`
	Alias        string `json:"alias"`
}

// UpsertFactSubject creates a canonical subject or updates its display name.
func UpsertFactSubject(ctx context.Context, endpoint, token string, in UpsertFactSubjectInput) (*FactSubject, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out struct {
		Subject FactSubject `json:"subject"`
	}
	requestURL := factSubjectsURL(endpoint) + "/" + url.PathEscape(in.CanonicalKey)
	if err := doJSON(ctx, http.MethodPut, requestURL, token, body, &out); err != nil {
		return nil, err
	}
	return &out.Subject, nil
}

// AddFactSubjectAlias adds one normalized lookup alias to a canonical subject.
func AddFactSubjectAlias(ctx context.Context, endpoint, token string, in AddFactSubjectAliasInput) (*FactSubject, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out struct {
		Subject FactSubject `json:"subject"`
	}
	requestURL := factSubjectsURL(endpoint) + "/" + url.PathEscape(in.CanonicalKey) + "/aliases"
	if err := doJSON(ctx, http.MethodPost, requestURL, token, body, &out); err != nil {
		return nil, err
	}
	return &out.Subject, nil
}

// ListFactSubjects returns the authenticated agent's canonical subject inventory.
func ListFactSubjects(ctx context.Context, endpoint, token string) ([]FactSubject, error) {
	var out struct {
		Subjects []FactSubject `json:"subjects"`
	}
	if err := doJSON(ctx, http.MethodGet, factSubjectsURL(endpoint), token, nil, &out); err != nil {
		return nil, err
	}
	return out.Subjects, nil
}

func factSubjectsURL(endpoint string) string {
	return strings.TrimRight(endpoint, "/") + "/v1/fact-subjects"
}
