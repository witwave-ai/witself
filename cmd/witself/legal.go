package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/jsonstrict"
	"github.com/witwave-ai/witself/internal/legal"
)

const maxConsentManifestBytes = 64 << 10

var consentManifestVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// legalCmd reads the published legal documents from the terminal — the same
// pages, at the same versions, that signup consent records. The documents
// are fetched from the published site rather than compiled in, so what the
// reader sees is exactly what is in force.
func legalCmd(args []string) int {
	fs := flag.NewFlagSet("legal", flag.ContinueOnError)
	base := fs.String("endpoint", legalBaseURL(), "base URL of the published legal pages")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: witself legal [DOCUMENT] [--endpoint URL]")
		fmt.Fprintln(os.Stderr, "  with no DOCUMENT: list the published documents and versions")
		fmt.Fprintln(os.Stderr, "  DOCUMENT: terms | privacy | acceptable-use | dpa | refunds")
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	rest := fs.Args()
	if len(rest) > 1 {
		fs.Usage()
		return 2
	}
	if len(rest) == 0 {
		return legalList(*base)
	}
	return legalShow(*base, rest[0])
}

func legalBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("WITSELF_LEGAL_URL")); v != "" {
		return v
	}
	return legal.BaseURL
}

func legalHTTPGet(rawURL, accept string) ([]byte, error) {
	return legalHTTPGetBounded(context.Background(), rawURL, accept, 1<<20)
}

// legalHTTPGetBounded reads public legal content without account credentials.
// Its deadline covers redirects and the complete body, and redirects must stay
// on the original authority (including scheme and port).
func legalHTTPGetBounded(ctx context.Context, rawURL, accept string, limit int64) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || !validLegalURL(parsed) {
		return nil, errors.New("invalid legal endpoint")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !validLegalURL(req.URL) ||
			req.URL.Scheme != parsed.Scheme || !strings.EqualFold(req.URL.Host, parsed.Host) {
			return errors.New("legal redirect left the original authority or exceeded the limit")
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := client.Do(req)
	if err != nil {
		// Transport errors can contain redirected URLs. Keep remote-controlled
		// values out of diagnostics and never print URL credentials.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("legal request interrupted: %w", ctx.Err())
		}
		return nil, errors.New("legal request failed or redirected outside its authority")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("legal endpoint returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, errors.New("legal response body could not be read completely")
	}
	if int64(len(body)) > limit {
		return nil, errors.New("legal response exceeds the size limit")
	}
	return body, nil
}

func validLegalURL(parsed *url.URL) bool {
	return parsed != nil && (parsed.Scheme == "https" || parsed.Scheme == "http") &&
		parsed.Hostname() != "" && parsed.User == nil && parsed.Opaque == "" && parsed.Fragment == ""
}

func signupLegalBase(controlPlane string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(controlPlane))
	if err != nil || !validLegalURL(parsed) || parsed.RawQuery != "" || parsed.ForceQuery {
		return "", errors.New("invalid control plane endpoint for legal consent")
	}
	parsed.Path, parsed.RawPath = "/legal", ""
	return parsed.String(), nil
}

// signupLegalVersions validates the exact served consent fields before the
// caller publishes a journal. Other manifest documents are not consent inputs.
func signupLegalVersions(ctx context.Context, base string) (string, string, error) {
	body, err := legalHTTPGetBounded(ctx, base+"/versions.json", "application/json", maxConsentManifestBytes)
	if err != nil {
		return "", "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := jsonstrict.ConsumeUniqueValue(decoder); err != nil {
		return "", "", errors.New("legal manifest must contain unique JSON fields")
	}
	if err := jsonstrict.RequireEOF(decoder); err != nil {
		return "", "", errors.New("legal manifest must contain one complete JSON object")
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(body, &manifest); err != nil {
		return "", "", errors.New("invalid legal manifest")
	}
	versions := make([]string, 0, 2)
	for _, slug := range []string{"terms", "privacy"} {
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(manifest[slug], &entry); err != nil {
			return "", "", fmt.Errorf("legal manifest lacks a valid %s entry", slug)
		}
		var version, path string
		if json.Unmarshal(entry["version"], &version) != nil ||
			!consentManifestVersionPattern.MatchString(version) ||
			json.Unmarshal(entry["path"], &path) != nil || path != "/legal/"+slug {
			return "", "", fmt.Errorf("legal manifest has an invalid %s version or path", slug)
		}
		versions = append(versions, version)
	}
	return versions[0], versions[1], nil
}

type legalManifestEntry struct {
	Title   string `json:"title"`
	Version string `json:"version"`
	Path    string `json:"path"`
}

func legalList(base string) int {
	body, err := legalHTTPGet(strings.TrimSuffix(base, "/")+"/versions.json", "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: fetch legal versions: %v\n", err)
		return 1
	}
	manifest := map[string]legalManifestEntry{}
	if err := json.Unmarshal(body, &manifest); err != nil {
		fmt.Fprintf(os.Stderr, "witself: parse legal versions: %v\n", err)
		return 1
	}
	slugs := make([]string, 0, len(manifest))
	for slug := range manifest {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	fmt.Println("DOCUMENT        VERSION      TITLE")
	for _, slug := range slugs {
		entry := manifest[slug]
		fmt.Printf("%-15s %-12s %s\n", slug, entry.Version, entry.Title)
	}
	fmt.Printf("\nread one: witself legal DOCUMENT · on the web: %s\n",
		strings.TrimSuffix(base, "/"))
	return 0
}

func legalShow(base, document string) int {
	slug := strings.TrimSpace(document)
	// "dpa" and "refunds" are the published slugs; accept the natural long
	// forms too so nobody has to guess.
	switch slug {
	case "terms-of-service":
		slug = "terms"
	case "privacy-policy":
		slug = "privacy"
	case "data-processing-addendum":
		slug = "dpa"
	case "refund-cancellation", "refunds-cancellation":
		slug = "refunds"
	}
	if !legalKnownSlug(slug) {
		fmt.Fprintf(os.Stderr,
			"witself: unknown document %q (terms | privacy | acceptable-use | dpa | refunds)\n",
			document)
		return 2
	}
	pageURL := strings.TrimSuffix(base, "/") + "/" + slug
	body, err := legalHTTPGet(pageURL+"?format=md", "text/markdown")
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: fetch %s: %v\n", slug, err)
		return 1
	}
	_, _ = os.Stdout.Write(body)
	if len(body) > 0 && body[len(body)-1] != '\n' {
		fmt.Println()
	}
	fmt.Printf("\n(published at %s)\n", pageURL)
	return 0
}

func legalKnownSlug(slug string) bool {
	switch slug {
	case "terms", "privacy", "acceptable-use", "dpa", "refunds":
		return true
	default:
		return false
	}
}
