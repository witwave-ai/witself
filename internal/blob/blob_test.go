package blob_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
