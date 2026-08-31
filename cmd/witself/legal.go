package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/witwave-ai/witself/internal/legal"
)

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
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, fmt.Errorf("invalid legal endpoint %q", rawURL)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d", parsed.String(), resp.StatusCode)
	}
	return body, nil
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
	os.Stdout.Write(body)
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
