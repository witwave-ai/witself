package blob_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/blob"
	"github.com/witwave-ai/witself/internal/blob/blobtest"
)

func newClient(t *testing.T) (*blob.Client, *blobtest.Server) {
	t.Helper()
	srv := blobtest.New(t)
	c, err := blob.New(blob.Config{
		Endpoint: srv.URL, Bucket: "test-bucket",
		AccessKey: "AKTEST", SecretKey: "secret",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

func TestPutGetRoundTrip(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()

	etag, err := c.Put(ctx, "a/b.json", []byte(`{"x":1}`), blob.Cond{})
	if err != nil || etag == "" {
		t.Fatalf("Put = %q, %v; want etag", etag, err)
	}
	data, gotETag, err := c.Get(ctx, "a/b.json")
	if err != nil || string(data) != `{"x":1}` || gotETag != etag {
		t.Fatalf("Get = %q, %q, %v; want the object back with the same etag", data, gotETag, err)
	}
	if _, _, err := c.Get(ctx, "a/missing.json"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Get missing = %v; want ErrNotFound", err)
	}
}

func TestGetBounded(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()

	etag, err := c.Put(ctx, "bounded/data", []byte("four"), blob.Cond{})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	data, gotETag, err := c.GetBounded(ctx, "bounded/data", 4)
	if err != nil || string(data) != "four" || gotETag != etag {
		t.Fatalf("GetBounded exact = %q, %q, %v; want body and etag", data, gotETag, err)
	}
	data, gotETag, err = c.GetBounded(ctx, "bounded/data", 3)
	if !errors.Is(err, blob.ErrLimitExceeded) || data != nil || gotETag != "" {
		t.Fatalf("GetBounded overflow = %q, %q, %v; want no partial result and ErrLimitExceeded", data, gotETag, err)
	}
	data, gotETag, err = c.GetBounded(ctx, "bounded/data", 0)
	if !errors.Is(err, blob.ErrLimitExceeded) || data != nil || gotETag != "" {
		t.Fatalf("GetBounded zero overflow = %q, %q, %v; want no partial result and ErrLimitExceeded", data, gotETag, err)
	}
	if _, _, err := c.GetBounded(ctx, "bounded/data", -1); err == nil {
		t.Fatal("GetBounded negative limit succeeded")
	}
	if _, _, err := c.GetBounded(ctx, "bounded/missing", 4); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("GetBounded missing = %v; want ErrNotFound", err)
	}

	if _, err := c.Put(ctx, "bounded/empty", nil, blob.Cond{}); err != nil {
		t.Fatalf("Put empty: %v", err)
	}
	data, _, err = c.GetBounded(ctx, "bounded/empty", 0)
	if err != nil || len(data) != 0 {
		t.Fatalf("GetBounded empty = %q, %v; want an empty body", data, err)
	}
}

func TestGetBoundedRequiresCanonicalQuotedETag(t *testing.T) {
	for _, etag := range []string{
		"", "unquoted", `"partial`, `partial"`, `""nested""`,
		`W/"weak"`, `"has space"`, "\"has\tcontrol\"",
	} {
		t.Run(fmt.Sprintf("etag_%q", etag), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if etag != "" {
					w.Header().Set("ETag", etag)
				}
				_, _ = w.Write([]byte("body"))
			}))
			t.Cleanup(srv.Close)
			client, err := blob.New(blob.Config{
				Endpoint: srv.URL, Bucket: "b",
				AccessKey: "AKTEST", SecretKey: "secret",
			})
			if err != nil {
				t.Fatal(err)
			}
			data, gotETag, err := client.GetBounded(
				context.Background(), "key", 4)
			if err == nil || data != nil || gotETag != "" {
				t.Fatalf("GetBounded malformed ETag = %q, %q, %v", data, gotETag, err)
			}
		})
	}
}

func TestGetBoundedRejectsMultipleETagHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("ETag", `"first"`)
		w.Header().Add("ETag", `"second"`)
		_, _ = w.Write([]byte("body"))
	}))
	t.Cleanup(srv.Close)
	client, err := blob.New(blob.Config{
		Endpoint: srv.URL, Bucket: "b",
		AccessKey: "AKTEST", SecretKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, etag, err := client.GetBounded(context.Background(), "key", 4)
	if err == nil || data != nil || etag != "" {
		t.Fatalf("GetBounded duplicate ETags = %q, %q, %v", data, etag, err)
	}
}

func TestGetRetainsPermissiveLegacyETagHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", "legacy-unquoted")
		_, _ = w.Write([]byte("body"))
	}))
	t.Cleanup(srv.Close)
	client, err := blob.New(blob.Config{
		Endpoint: srv.URL, Bucket: "b",
		AccessKey: "AKTEST", SecretKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, etag, err := client.Get(context.Background(), "key")
	if err != nil || string(data) != "body" || etag != "legacy-unquoted" {
		t.Fatalf("legacy Get = %q, %q, %v", data, etag, err)
	}
}

func TestConditionalWrites(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()

	// Create-only succeeds once.
	if _, err := c.Put(ctx, "k", []byte("v1"), blob.Cond{IfNoneMatchAny: true}); err != nil {
		t.Fatalf("create-only Put: %v", err)
	}
	if _, err := c.Put(ctx, "k", []byte("v2"), blob.Cond{IfNoneMatchAny: true}); !errors.Is(err, blob.ErrPrecondition) {
		t.Fatalf("second create-only Put = %v; want ErrPrecondition", err)
	}

	// Compare-and-swap: the stale etag loses, the fresh one wins.
	_, etag, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := c.Put(ctx, "k", []byte("v2"), blob.Cond{IfMatch: etag}); err != nil {
		t.Fatalf("CAS Put with fresh etag: %v", err)
	}
	if _, err := c.Put(ctx, "k", []byte("v3"), blob.Cond{IfMatch: etag}); !errors.Is(err, blob.ErrPrecondition) {
		t.Fatalf("CAS Put with stale etag = %v; want ErrPrecondition — this is the no-database design's load-bearing behavior", err)
	}
}

func TestListPagination(t *testing.T) {
	c, srv := newClient(t)
	srv.PageSize = 2 // force the pagination loop
	ctx := context.Background()

	for _, k := range []string{"p/1", "p/2", "p/3", "p/4", "p/5", "other/x"} {
		if _, err := c.Put(ctx, k, []byte("v"), blob.Cond{}); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	keys, err := c.List(ctx, "p/")
	if err != nil || len(keys) != 5 {
		t.Fatalf("List = %v, %v; want the 5 p/ keys across 3 pages", keys, err)
	}
}

func TestListCompletePaginationAndMetadata(t *testing.T) {
	c, srv := newClient(t)
	srv.PageSize = 2
	ctx := context.Background()

	want := make([]blob.ObjectInfo, 0, 5)
	for _, item := range []struct {
		key  string
		body string
	}{
		{key: "p/1", body: "a"},
		{key: "p/2", body: "bb"},
		{key: "p/3", body: "ccc"},
		{key: "p/4", body: "dddd"},
		{key: "p/5", body: "eeeee"},
		{key: "other/x", body: "ignored"},
	} {
		etag, err := c.Put(ctx, item.key, []byte(item.body), blob.Cond{})
		if err != nil {
			t.Fatalf("Put %s: %v", item.key, err)
		}
		if strings.HasPrefix(item.key, "p/") {
			want = append(want, blob.ObjectInfo{Key: item.key, ETag: etag, Size: int64(len(item.body))})
		}
	}

	got, err := c.ListComplete(ctx, "p/", 5)
	if err != nil {
		t.Fatalf("ListComplete: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListComplete = %#v; want %#v", got, want)
	}
	if got, err := c.ListComplete(ctx, "missing/", 0); err != nil || len(got) != 0 {
		t.Fatalf("ListComplete empty = %#v, %v; want empty success", got, err)
	}
	if _, err := c.ListComplete(ctx, "p/", -1); err == nil {
		t.Fatal("ListComplete negative limit succeeded")
	}
	if got, err := c.ListComplete(ctx, "p/", 4); !errors.Is(err, blob.ErrLimitExceeded) || got != nil {
		t.Fatalf("ListComplete overflow = %#v, %v; want no partial result and ErrLimitExceeded", got, err)
	}
}

func TestListCompleteRejectsAmbiguousPagination(t *testing.T) {
	tests := []struct {
		name  string
		pages map[string]string
	}{
		{
			name: "truncated without token",
			pages: map[string]string{"": listPageXML(
				[]testListedObject{{key: "p/a", etag: "a", size: "1"}}, true, "")},
		},
		{
			name: "truncated with control token",
			pages: map[string]string{"": listPageXML(
				[]testListedObject{{key: "p/a", etag: "a", size: "1"}}, true, "bad\ttoken")},
		},
		{
			name: "immediately repeated token",
			pages: map[string]string{
				"":       listPageXML([]testListedObject{{key: "p/a", etag: "a", size: "1"}}, true, "repeat"),
				"repeat": listPageXML([]testListedObject{{key: "p/b", etag: "b", size: "1"}}, true, "repeat"),
			},
		},
		{
			name: "repeated prior token",
			pages: map[string]string{
				"":    listPageXML([]testListedObject{{key: "p/a", etag: "a", size: "1"}}, true, "one"),
				"one": listPageXML([]testListedObject{{key: "p/b", etag: "b", size: "1"}}, true, "two"),
				"two": listPageXML([]testListedObject{{key: "p/c", etag: "c", size: "1"}}, true, "one"),
			},
		},
		{
			name:  "truncated empty page",
			pages: map[string]string{"": listPageXML(nil, true, "next")},
		},
		{
			name: "continuation resolves empty",
			pages: map[string]string{
				"":     listPageXML([]testListedObject{{key: "p/a", etag: "a", size: "1"}}, true, "next"),
				"next": listPageXML(nil, false, ""),
			},
		},
		{
			name: "duplicate across pages",
			pages: map[string]string{
				"":     listPageXML([]testListedObject{{key: "p/a", etag: "a", size: "1"}}, true, "next"),
				"next": listPageXML([]testListedObject{{key: "p/a", etag: "b", size: "1"}}, false, ""),
			},
		},
		{
			name: "non-increasing across pages",
			pages: map[string]string{
				"":     listPageXML([]testListedObject{{key: "p/b", etag: "b", size: "1"}}, true, "next"),
				"next": listPageXML([]testListedObject{{key: "p/a", etag: "a", size: "1"}}, false, ""),
			},
		},
		{
			name: "complete page has next token",
			pages: map[string]string{"": listPageXML(
				[]testListedObject{{key: "p/a", etag: "a", size: "1"}}, false, "unexpected")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newScriptedListClient(t, tt.pages)
			if got, err := c.ListComplete(context.Background(), "p/", 10); !errors.Is(err, blob.ErrInvalidListing) || got != nil {
				t.Fatalf("ListComplete = %#v, %v; want no result and ErrInvalidListing", got, err)
			}
		})
	}
}

func TestListCompleteRejectsDuplicateProtocolFields(t *testing.T) {
	tests := []struct {
		name string
		xml  string
	}{
		{
			name: "duplicate truncation can not hide a continuation",
			xml: `<ListBucketResult>` +
				`<Contents><Key>p/a</Key><ETag>&quot;a&quot;</ETag><Size>1</Size></Contents>` +
				`<IsTruncated>true</IsTruncated><IsTruncated>false</IsTruncated>` +
				`</ListBucketResult>`,
		},
		{
			name: "duplicate continuation token",
			xml: `<ListBucketResult>` +
				`<Contents><Key>p/a</Key><ETag>&quot;a&quot;</ETag><Size>1</Size></Contents>` +
				`<IsTruncated>true</IsTruncated>` +
				`<NextContinuationToken>one</NextContinuationToken>` +
				`<NextContinuationToken>two</NextContinuationToken>` +
				`</ListBucketResult>`,
		},
		{
			name: "duplicate object key",
			xml: `<ListBucketResult><Contents>` +
				`<Key>outside/a</Key><Key>p/a</Key>` +
				`<ETag>&quot;a&quot;</ETag><Size>1</Size>` +
				`</Contents><IsTruncated>false</IsTruncated></ListBucketResult>`,
		},
		{
			name: "duplicate object etag",
			xml: `<ListBucketResult><Contents>` +
				`<Key>p/a</Key><ETag>&quot;a&quot;</ETag><ETag>&quot;b&quot;</ETag><Size>1</Size>` +
				`</Contents><IsTruncated>false</IsTruncated></ListBucketResult>`,
		},
		{
			name: "duplicate object size",
			xml: `<ListBucketResult><Contents>` +
				`<Key>p/a</Key><ETag>&quot;a&quot;</ETag><Size>1</Size><Size>2</Size>` +
				`</Contents><IsTruncated>false</IsTruncated></ListBucketResult>`,
		},
		{
			name: "nested scalar content",
			xml: `<ListBucketResult><Contents>` +
				`<Key><Value>p/a</Value></Key>` +
				`<ETag>&quot;a&quot;</ETag><Size>1</Size>` +
				`</Contents><IsTruncated>false</IsTruncated></ListBucketResult>`,
		},
		{
			name: "missing truncation control",
			xml: `<ListBucketResult><Contents>` +
				`<Key>p/a</Key><ETag>&quot;a&quot;</ETag><Size>1</Size>` +
				`</Contents></ListBucketResult>`,
		},
		{
			name: "noncanonical truncation control",
			xml:  `<ListBucketResult><IsTruncated>False</IsTruncated></ListBucketResult>`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newScriptedListClient(t, map[string]string{"": tt.xml})
			got, err := c.ListComplete(context.Background(), "p/", 10)
			if !errors.Is(err, blob.ErrInvalidListing) || got != nil {
				t.Fatalf("ListComplete = %#v, %v; want no result and ErrInvalidListing", got, err)
			}
		})
	}
}

func TestListCompleteRejectsInvalidKeysAndMetadata(t *testing.T) {
	tests := []struct {
		name    string
		objects []testListedObject
	}{
		{name: "duplicate", objects: []testListedObject{
			{key: "p/a", etag: "a", size: "1"}, {key: "p/a", etag: "b", size: "1"},
		}},
		{name: "non-increasing", objects: []testListedObject{
			{key: "p/b", etag: "b", size: "1"}, {key: "p/a", etag: "a", size: "1"},
		}},
		{name: "outside prefix", objects: []testListedObject{{key: "other/a", etag: "a", size: "1"}}},
		{name: "empty key", objects: []testListedObject{{key: "", etag: "a", size: "1"}}},
		{name: "missing etag", objects: []testListedObject{{key: "p/a", size: "1"}}},
		{name: "missing size", objects: []testListedObject{{key: "p/a", etag: "a"}}},
		{name: "negative size", objects: []testListedObject{{key: "p/a", etag: "a", size: "-1"}}},
		{name: "size overflow", objects: []testListedObject{{key: "p/a", etag: "a", size: "9223372036854775808"}}},
		{name: "noncanonical size", objects: []testListedObject{{key: "p/a", etag: "a", size: "01"}}},
		{name: "oversized key", objects: []testListedObject{{key: "p/" + strings.Repeat("a", 1023), etag: "a", size: "1"}}},
		{name: "oversized etag", objects: []testListedObject{{key: "p/a", etag: strings.Repeat("a", 257), size: "1"}}},
		{name: "etag with whitespace", objects: []testListedObject{{key: "p/a", etag: "has space", size: "1"}}},
		{name: "etag with control", objects: []testListedObject{{key: "p/a", etag: "has\tcontrol", size: "1"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newScriptedListClient(t, map[string]string{"": listPageXML(tt.objects, false, "")})
			got, err := c.ListComplete(context.Background(), "p/", 10)
			if !errors.Is(err, blob.ErrInvalidListing) || got != nil {
				t.Fatalf("ListComplete = %#v, %v; want no result and ErrInvalidListing", got, err)
			}
		})
	}
}

func TestListCompleteRejectsOversizedContinuationToken(t *testing.T) {
	page := listPageXML(
		[]testListedObject{{key: "p/a", etag: "a", size: "1"}}, true,
		strings.Repeat("t", 8_193))
	c := newScriptedListClient(t, map[string]string{"": page})
	got, err := c.ListComplete(context.Background(), "p/", 10)
	if !errors.Is(err, blob.ErrInvalidListing) || got != nil {
		t.Fatalf("ListComplete oversized token = %#v, %v; want no result and ErrInvalidListing", got, err)
	}
}

func TestListCompleteRejectsTooManyObjectsInOnePage(t *testing.T) {
	objects := make([]testListedObject, 1_001)
	for index := range objects {
		objects[index] = testListedObject{
			key: fmt.Sprintf("p/%04d", index), etag: "a", size: "1",
		}
	}
	c := newScriptedListClient(t, map[string]string{
		"": listPageXML(objects, false, ""),
	})
	got, err := c.ListComplete(context.Background(), "p/", len(objects))
	if !errors.Is(err, blob.ErrInvalidListing) || got != nil {
		t.Fatalf("ListComplete oversized object page = %#v, %v; want no result and ErrInvalidListing", got, err)
	}
}

func TestListCompleteRejectsOversizedPage(t *testing.T) {
	// The strict API caps each provider response independently of maxKeys, so
	// malformed XML cannot turn a read-only activation audit into an unbounded
	// allocation. Keep this one byte beyond the package's documented 16 MiB
	// defensive page boundary.
	c := newScriptedListClient(t, map[string]string{"": strings.Repeat("x", (16<<20)+1)})
	got, err := c.ListComplete(context.Background(), "p/", 10)
	if !errors.Is(err, blob.ErrLimitExceeded) || got != nil {
		t.Fatalf("ListComplete oversized page = %#v, %v; want no result and ErrLimitExceeded", got, err)
	}
}

func TestListRetainsPermissiveMissingTokenBehavior(t *testing.T) {
	c := newScriptedListClient(t, map[string]string{"": listPageXML(
		[]testListedObject{{key: "p/a", etag: "a", size: "1"}}, true, "")})
	got, err := c.List(context.Background(), "p/")
	if err != nil || !reflect.DeepEqual(got, []string{"p/a"}) {
		t.Fatalf("List = %#v, %v; want historical permissive result", got, err)
	}
}

type testListedObject struct {
	key  string
	etag string
	size string
}

func listPageXML(objects []testListedObject, truncated bool, next string) string {
	var b strings.Builder
	b.WriteString(`<ListBucketResult>`)
	for _, object := range objects {
		b.WriteString(`<Contents><Key>`)
		b.WriteString(object.key)
		b.WriteString(`</Key>`)
		if object.etag != "" {
			b.WriteString(`<ETag>&quot;`)
			b.WriteString(object.etag)
			b.WriteString(`&quot;</ETag>`)
		}
		if object.size != "" {
			b.WriteString(`<Size>`)
			b.WriteString(object.size)
			b.WriteString(`</Size>`)
		}
		b.WriteString(`</Contents>`)
	}
	b.WriteString(`<IsTruncated>`)
	b.WriteString(fmt.Sprintf("%t", truncated))
	b.WriteString(`</IsTruncated>`)
	if next != "" {
		b.WriteString(`<NextContinuationToken>`)
		b.WriteString(next)
		b.WriteString(`</NextContinuationToken>`)
	}
	b.WriteString(`</ListBucketResult>`)
	return b.String()
}

func newScriptedListClient(t *testing.T, pages map[string]string) *blob.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, ok := pages[r.URL.Query().Get("continuation-token")]
		if !ok {
			t.Errorf("unexpected continuation token %q", r.URL.Query().Get("continuation-token"))
			http.Error(w, "unexpected token", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(srv.Close)
	c, err := blob.New(blob.Config{
		Endpoint: srv.URL, Bucket: "b",
		AccessKey: "AKTEST", SecretKey: "secret",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestMisconfiguredBucketIsLoud: NoSuchBucket must NOT read as "object
// absent" — a typo'd bucket would otherwise silently serve free/free records
// and ACK webhook events that never land.
func TestMisconfiguredBucketIsLoud(t *testing.T) {
	srv := blobtest.New(t)
	srv.OnlyBucket = "right-bucket"
	c, err := blob.New(blob.Config{
		Endpoint: srv.URL, Bucket: "wrong-bucket",
		AccessKey: "AKTEST", SecretKey: "secret",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = c.Get(context.Background(), "any/key")
	if errors.Is(err, blob.ErrNotFound) {
		t.Fatal("NoSuchBucket classified as ErrNotFound — misconfiguration must be loud")
	}
	if err == nil || !strings.Contains(err.Error(), "NoSuchBucket") {
		t.Fatalf("err = %v; want a loud NoSuchBucket error", err)
	}
}

// TestConditionalConflict409: AWS S3 reports a lost concurrent conditional
// write as 409 ConditionalRequestConflict; it must map to ErrPrecondition so
// the Manager's retry loop converges instead of aborting.
func TestConditionalConflict409(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`<Error><Code>ConditionalRequestConflict</Code><Message>retry</Message></Error>`))
	}))
	t.Cleanup(srv.Close)
	c, err := blob.New(blob.Config{
		Endpoint: srv.URL, Bucket: "b",
		AccessKey: "AKTEST", SecretKey: "secret",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Put(context.Background(), "k", []byte("v"), blob.Cond{IfMatch: "etag"})
	if !errors.Is(err, blob.ErrPrecondition) {
		t.Fatalf("409 ConditionalRequestConflict = %v; want ErrPrecondition", err)
	}
}
