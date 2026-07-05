// Package blobtest is an in-process S3-compatible stub for tests: objects in
// memory, real ETag + conditional-write semantics (If-Match / If-None-Match),
// and ListObjectsV2 with pagination. It lets the R2-backed registry be tested
// hermetically — including the 412-on-stale behavior the design leans on —
// without network or credentials.
package blobtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

type object struct {
	data []byte
	etag string
}

// s3ErrorXML writes an S3-style XML error document — the shape real R2/S3
// return, and what the client's error classification parses.
func s3ErrorXML(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, msg)
}

// Server is the stub. URL is the endpoint to point a blob.Client at.
type Server struct {
	URL string
	// PageSize caps ListObjectsV2 pages so tests exercise pagination.
	PageSize int
	// OnlyBucket, when set, makes every other bucket a NoSuchBucket 404 —
	// the misconfigured-deployment case the client must classify loudly.
	OnlyBucket string

	mu      sync.Mutex
	objects map[string]object // "<bucket>/<key>" -> object
	ts      *httptest.Server
}

// New starts a stub server; it shuts down with the test.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{objects: map[string]object{}, PageSize: 1000}
	s.ts = httptest.NewServer(http.HandlerFunc(s.handle))
	s.URL = s.ts.URL
	t.Cleanup(s.ts.Close)
	return s
}

// Len reports how many objects exist (assertion helper).
func (s *Server) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") == "" || r.Header.Get("x-amz-content-sha256") == "" {
		http.Error(w, "missing signature", http.StatusForbidden)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket == "" {
		http.Error(w, "no bucket", http.StatusBadRequest)
		return
	}
	if s.OnlyBucket != "" && bucket != s.OnlyBucket {
		s3ErrorXML(w, http.StatusNotFound, "NoSuchBucket", "the specified bucket does not exist")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// ListObjectsV2: GET on the bucket with list-type=2.
	if key == "" && r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		s.list(w, r, bucket)
		return
	}

	full := bucket + "/" + key
	switch r.Method {
	case http.MethodGet:
		o, ok := s.objects[full]
		if !ok {
			s3ErrorXML(w, http.StatusNotFound, "NoSuchKey", "the specified key does not exist")
			return
		}
		w.Header().Set("ETag", `"`+o.etag+`"`)
		_, _ = w.Write(o.data)
	case http.MethodPut:
		cur, exists := s.objects[full]
		if r.Header.Get("If-None-Match") == "*" && exists {
			http.Error(w, "exists", http.StatusPreconditionFailed)
			return
		}
		if im := strings.Trim(r.Header.Get("If-Match"), `"`); im != "" {
			if !exists || cur.etag != im {
				http.Error(w, "etag mismatch", http.StatusPreconditionFailed)
				return
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256(body)
		o := object{data: body, etag: hex.EncodeToString(sum[:8])}
		s.objects[full] = o
		w.Header().Set("ETag", `"`+o.etag+`"`)
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, bucket string) {
	prefix := r.URL.Query().Get("prefix")
	after := r.URL.Query().Get("continuation-token")
	var keys []string
	for full := range s.objects {
		b, k, _ := strings.Cut(full, "/")
		if b == bucket && strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if after != "" {
		i := sort.SearchStrings(keys, after)
		if i < len(keys) && keys[i] == after {
			i++
		}
		keys = keys[i:]
	}
	truncated := len(keys) > s.PageSize
	next := ""
	if truncated {
		keys = keys[:s.PageSize]
		next = keys[len(keys)-1]
	}
	type contents struct {
		Key string `xml:"Key"`
	}
	out := struct {
		XMLName               xml.Name   `xml:"ListBucketResult"`
		Contents              []contents `xml:"Contents"`
		IsTruncated           bool       `xml:"IsTruncated"`
		NextContinuationToken string     `xml:"NextContinuationToken,omitempty"`
	}{IsTruncated: truncated, NextContinuationToken: next}
	for _, k := range keys {
		out.Contents = append(out.Contents, contents{Key: k})
	}
	w.Header().Set("Content-Type", "application/xml")
	if err := xml.NewEncoder(w).Encode(out); err != nil {
		fmt.Println("blobtest: encode list:", err)
	}
}
