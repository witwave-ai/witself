package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/signuplegal"
)

// SignupLegalRefusalError is returned only for a complete typed 409 received
// from the selected signup endpoint and bound to the outstanding request.
type SignupLegalRefusalError struct{ Refusal signuplegal.Refusal }

func (e *SignupLegalRefusalError) Error() string { return signuplegal.RefusalError }
func (e *SignupLegalRefusalError) Unwrap() error { return ErrConflict }

func signupMutationURL(endpoint, suffix string) (string, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	u, err := url.Parse(endpoint)
	if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") ||
		u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.HasSuffix(u.Host, ":") {
		return "", errors.New("invalid control plane endpoint")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid control plane endpoint")
		}
	}
	return endpoint + suffix, nil
}

func signupHTTPClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		// Even a same-authority redirect could repeat a mutation at another path.
		return http.ErrUseLastResponse
	}}
}

func signupResponseMatches(resp *http.Response, target string) bool {
	want, err := url.Parse(target)
	if err != nil || resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return false
	}
	got := resp.Request.URL
	return got.User == nil && got.Opaque == "" && got.Fragment == "" &&
		got.Scheme == want.Scheme && strings.EqualFold(got.Host, want.Host) &&
		got.EscapedPath() == want.EscapedPath() && got.RawQuery == want.RawQuery && got.ForceQuery == want.ForceQuery
}

func readSignupLegalBody(resp *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, signuplegal.MaxProtocolBytes+1))
	if err != nil {
		return nil, errors.New("signup legal response body could not be read completely")
	}
	if resp.Request != nil && resp.Request.Context().Err() != nil {
		return nil, errors.New("signup legal response was interrupted")
	}
	if len(body) > signuplegal.MaxProtocolBytes {
		return nil, errors.New("signup legal response exceeds the size limit")
	}
	return body, nil
}

func signupLegalRefusal(resp *http.Response, provisionID, terms, privacy string) error {
	body, err := readSignupLegalBody(resp)
	if err != nil {
		return err
	}
	refusal, err := signuplegal.DecodeRefusal(body)
	if err != nil {
		// Preserve ordinary conflict diagnostics, but they are never durable
		// refusal evidence. The complete transport read above stays mandatory.
		responseCopy := *resp
		responseCopy.Body = io.NopCloser(bytes.NewReader(body))
		return responseError(&responseCopy, "account creation conflict without valid legal refusal")
	}
	if err := refusal.ValidateFor(provisionID, terms, privacy); err != nil {
		return err
	}
	return &SignupLegalRefusalError{Refusal: refusal}
}

// RegisterAccountReconsent registers only the caller's already-persisted
// transition. An ambiguous result leaves that same transition pending; this
// function never creates a candidate, chooses consent or begins signup.
func RegisterAccountReconsent(ctx context.Context, endpoint string, request signuplegal.ReconsentRequest) (*signuplegal.Ack, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	target, err := signupMutationURL(endpoint, "/v1/account-signups/"+request.Refusal.ProvisionID+":reconsent")
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid signup reconsent request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := signupHTTPClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("signup reconsent interrupted: %w", ctx.Err())
		}
		return nil, errors.New("signup reconsent request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if !signupResponseMatches(resp, target) {
		return nil, errors.New("signup reconsent response left the selected endpoint")
	}
	response, err := readSignupLegalBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusConflict {
			return nil, fmt.Errorf("signup reconsent conflict: %w", ErrConflict)
		}
		return nil, fmt.Errorf("signup reconsent returned HTTP %d", resp.StatusCode)
	}
	ack, err := signuplegal.DecodeAck(response)
	if err != nil {
		return nil, err
	}
	if err := ack.ValidateFor(request); err != nil {
		return nil, err
	}
	return &ack, nil
}
