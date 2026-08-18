// Package blob is a minimal S3-compatible object client — just enough surface
// for the control plane's R2-backed registry: Get (with ETag), conditional Put
// (If-Match / If-None-Match), and prefix List. It signs requests with AWS
// Signature v4 using only the standard library, so the lean root module never
// grows an AWS SDK dependency (the same discipline that put Pulumi in a nested
// module).
//
// It speaks to any S3-compatible endpoint (Cloudflare R2, AWS S3, MinIO) using
// path-style addressing. The conditional-write semantics are the load-bearing
// part: Put with IfMatch enforces compare-and-swap AT THE STORAGE LAYER (a
// stale writer gets 412 -> ErrPrecondition), which is what lets the registry
// run without a database.
package blob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned by Get for a missing key.
var ErrNotFound = errors.New("blob: not found")

// ErrPrecondition is returned by Put when the conditional write lost: the
// object changed since the ETag was read (IfMatch), or it already exists
// (IfNoneMatchAny). This is the storage-enforced compare-and-swap failure.
var ErrPrecondition = errors.New("blob: precondition failed")

// ErrLimitExceeded is returned by bounded reads and complete listings when
// the caller's explicit safety limit would be exceeded. No partial object or
// listing is returned with this error.
var ErrLimitExceeded = errors.New("blob: limit exceeded")

// ErrInvalidListing is returned by ListComplete when the object service
// returns a response that cannot prove a complete, consistently ordered
// prefix walk. The ordinary List method intentionally retains its historical
// permissive behavior for non-activation callers.
var ErrInvalidListing = errors.New("blob: invalid listing")

// ObjectInfo is stable object metadata returned by ListComplete. ETag is
// normalized without surrounding quotes, matching the ETag returned by Get.
type ObjectInfo struct {
	Key  string
	ETag string
	Size int64
}

// Cond expresses a conditional write. Zero value = unconditional.
type Cond struct {
	// IfMatch makes the Put succeed only if the object's current ETag equals
	// this value (compare-and-swap on an existing object).
	IfMatch string
	// IfNoneMatchAny makes the Put succeed only if the object does NOT exist
	// (create-only).
	IfNoneMatchAny bool
}

// Config connects a Client to one bucket on an S3-compatible endpoint.
type Config struct {
	// Endpoint is the scheme+host, e.g. https://<account>.r2.cloudflarestorage.com
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	// Region is the SigV4 region; R2 uses "auto". Defaults to "auto".
	Region string
	// HTTPClient defaults to http.DefaultClient.
	HTTPClient *http.Client
	// Now injects a clock for signing tests. Defaults to time.Now.
	Now func() time.Time
}

// Client is a minimal S3-compatible client bound to one bucket. Safe for
// concurrent use.
type Client struct {
	cfg  Config
	base *url.URL
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("blob: Endpoint, Bucket, AccessKey, and SecretKey are all required")
	}
	if cfg.Region == "" {
		cfg.Region = "auto"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	base, err := url.Parse(cfg.Endpoint)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("blob: invalid endpoint %q", cfg.Endpoint)
	}
	return &Client{cfg: cfg, base: base}, nil
}

// Get fetches the object and its ETag. ErrNotFound for missing keys.
func (c *Client) Get(ctx context.Context, key string) (data []byte, etag string, err error) {
	return c.get(ctx, key, -1)
}

// GetBounded fetches the object and its ETag while refusing a body larger
// than maxBytes. maxBytes may be zero (only an empty object fits) and must not
// be negative. On overflow it returns ErrLimitExceeded and no partial body.
func (c *Client) GetBounded(ctx context.Context, key string, maxBytes int64) (data []byte, etag string, err error) {
	if maxBytes < 0 {
		return nil, "", fmt.Errorf("blob: get %s: maxBytes must be non-negative", key)
	}
	return c.get(ctx, key, maxBytes)
}

// get implements Get and GetBounded. A negative maxBytes means unbounded and
// is used only by the existing Get API.
func (c *Client) get(ctx context.Context, key string, maxBytes int64) (data []byte, etag string, err error) {
	resp, err := c.do(ctx, http.MethodGet, key, nil, nil, nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		etag := strings.Trim(resp.Header.Get("ETag"), `"`)
		if maxBytes >= 0 {
			etagValues := resp.Header.Values("ETag")
			if len(etagValues) == 1 {
				etag, err = normalizeCompleteListETag(etagValues[0])
			} else {
				err = errors.New("ambiguous ETag header")
			}
			if err != nil {
				return nil, "", fmt.Errorf("blob: get %s returned an invalid ETag", key)
			}
		}
		var b []byte
		if maxBytes < 0 {
			b, err = io.ReadAll(resp.Body)
		} else {
			b, err = readAtMost(resp.Body, maxBytes)
			if errors.Is(err, ErrLimitExceeded) {
				return nil, "", fmt.Errorf("%w: get %s exceeds %d bytes", ErrLimitExceeded, key, maxBytes)
			}
		}
		if err != nil {
			return nil, "", fmt.Errorf("blob: read %s: %w", key, err)
		}
		return b, etag, nil
	case http.StatusNotFound:
		// A 404 is only "the object is absent" when the body says NoSuchKey.
		// NoSuchBucket is ALSO a 404, and conflating them is dangerous: a
		// misconfigured bucket would read as "every record absent", the
		// webhook path would ACK events for "unknown customers", and the
		// provider would never redeliver — a paid event permanently dropped.
		// Misconfiguration must be loud, not absent.
		if e := readS3Error(resp); e.Code != "NoSuchKey" {
			return nil, "", fmt.Errorf("blob: get %s: %s: %s %s", key, resp.Status, e.Code, e.Message)
		}
		return nil, "", ErrNotFound
	default:
		return nil, "", httpError("get", key, resp)
	}
}

// readAtMost reads at most maxBytes plus a one-byte overflow probe without
// computing maxBytes+1 (which could overflow int64). The probe is necessary
// for chunked responses whose Content-Length is unknown.
func readAtMost(r io.Reader, maxBytes int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBytes))
	if err != nil {
		return nil, err
	}
	var probe [1]byte
	n, err := io.ReadFull(r, probe[:])
	if n > 0 {
		return nil, ErrLimitExceeded
	}
	if errors.Is(err, io.EOF) {
		return b, nil
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Put writes the object under cond and returns the new ETag.
// ErrPrecondition when the conditional write lost.
func (c *Client) Put(ctx context.Context, key string, data []byte, cond Cond) (etag string, err error) {
	hdrs := map[string]string{}
	if cond.IfMatch != "" {
		hdrs["if-match"] = `"` + cond.IfMatch + `"`
	}
	if cond.IfNoneMatchAny {
		hdrs["if-none-match"] = "*"
	}
	resp, err := c.do(ctx, http.MethodPut, key, nil, data, hdrs)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return strings.Trim(resp.Header.Get("ETag"), `"`), nil
	case http.StatusPreconditionFailed:
		return "", ErrPrecondition
	case http.StatusConflict:
		// AWS S3 reports a LOST concurrent conditional write as
		// 409 ConditionalRequestConflict (its documented remedy: re-read the
		// ETag and retry — exactly our ErrStale path). R2 uses plain 412, but
		// this package promises S3 compatibility, and turning benign
		// contention into a hard failure would abort the Manager's retry
		// loop precisely where it exists to converge.
		if e := readS3Error(resp); e.Code == "ConditionalRequestConflict" {
			return "", ErrPrecondition
		}
		return "", httpError("put", key, resp)
	default:
		return "", httpError("put", key, resp)
	}
}

// List returns every key under prefix, following pagination.
func (c *Client) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	token := ""
	for {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("prefix", prefix)
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, "", q, nil, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			defer func() { _ = resp.Body.Close() }()
			return nil, httpError("list", prefix, resp)
		}
		var page struct {
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("blob: list %s: %w", prefix, err)
		}
		for _, o := range page.Contents {
			keys = append(keys, o.Key)
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return keys, nil
		}
		token = page.NextContinuationToken
	}
}

const (
	maxCompleteListPageBytes      int64 = 16 << 20
	maxCompleteListAggregateBytes int64 = 1 << 30
	maxCompleteListPages                = 8_192
	maxCompleteListObjectsPerPage       = 1_000
	maxCompleteListKeyBytes             = 1_024
	maxCompleteListETagBytes            = 256
	maxCompleteListTokenBytes           = 8_192
)

type listPage struct {
	Contents              []ObjectInfo
	IsTruncated           bool
	NextContinuationToken string
}

// ListComplete returns a bounded, stable-metadata inventory under prefix.
// Unlike List, it treats any pagination ambiguity as an error: every
// truncated page must advance a unique continuation token, every returned key
// must remain under prefix and be globally strictly increasing, and the total
// may not exceed maxKeys. Each page body is also bounded defensively.
//
// maxKeys may be zero (only an empty listing fits) and must not be negative.
func (c *Client) ListComplete(ctx context.Context, prefix string, maxKeys int) ([]ObjectInfo, error) {
	if maxKeys < 0 {
		return nil, fmt.Errorf("blob: list %s: maxKeys must be non-negative", prefix)
	}

	objects := make([]ObjectInfo, 0)
	seenTokens := map[string]struct{}{}
	token := ""
	previousKey := ""
	var aggregatePageBytes int64
	pages := 0
	for {
		pages++
		if pages > maxCompleteListPages {
			return nil, fmt.Errorf("%w: list %s exceeds %d pages", ErrLimitExceeded, prefix, maxCompleteListPages)
		}
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("prefix", prefix)
		q.Set("max-keys", "1000")
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, "", q, nil, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			err := httpError("list", prefix, resp)
			_ = resp.Body.Close()
			return nil, err
		}

		pageBody, readErr := readAtMost(resp.Body, maxCompleteListPageBytes)
		_ = resp.Body.Close()
		if errors.Is(readErr, ErrLimitExceeded) {
			return nil, fmt.Errorf("%w: list %s page exceeds %d bytes", ErrLimitExceeded, prefix, maxCompleteListPageBytes)
		}
		if readErr != nil {
			return nil, fmt.Errorf("blob: list %s: %w", prefix, readErr)
		}
		if int64(len(pageBody)) > maxCompleteListAggregateBytes-aggregatePageBytes {
			return nil, fmt.Errorf("%w: list %s exceeds %d response bytes", ErrLimitExceeded, prefix, maxCompleteListAggregateBytes)
		}
		aggregatePageBytes += int64(len(pageBody))
		page, err := decodeCompleteListPage(pageBody)
		if err != nil {
			return nil, fmt.Errorf("%w: list %s response: %v", ErrInvalidListing, prefix, err)
		}
		if token != "" && len(page.Contents) == 0 {
			return nil, fmt.Errorf("%w: list %s returned an empty continuation page", ErrInvalidListing, prefix)
		}

		for _, listed := range page.Contents {
			if listed.Key == "" || !strings.HasPrefix(listed.Key, prefix) {
				return nil, fmt.Errorf("%w: list %s returned a key outside the requested prefix", ErrInvalidListing, prefix)
			}
			if previousKey != "" && listed.Key <= previousKey {
				return nil, fmt.Errorf("%w: list %s returned duplicate or non-increasing keys", ErrInvalidListing, prefix)
			}
			if len(objects) >= maxKeys {
				return nil, fmt.Errorf("%w: list %s exceeds %d keys", ErrLimitExceeded, prefix, maxKeys)
			}
			objects = append(objects, listed)
			previousKey = listed.Key
		}

		if !page.IsTruncated {
			if page.NextContinuationToken != "" {
				return nil, fmt.Errorf("%w: list %s returned a continuation token on a complete page", ErrInvalidListing, prefix)
			}
			return objects, nil
		}
		if page.NextContinuationToken == "" {
			return nil, fmt.Errorf("%w: list %s returned a truncated page without a continuation token", ErrInvalidListing, prefix)
		}
		if len(page.Contents) == 0 {
			return nil, fmt.Errorf("%w: list %s returned a truncated page without keys", ErrInvalidListing, prefix)
		}
		if page.NextContinuationToken == token {
			return nil, fmt.Errorf("%w: list %s repeated its continuation token", ErrInvalidListing, prefix)
		}
		if _, exists := seenTokens[page.NextContinuationToken]; exists {
			return nil, fmt.Errorf("%w: list %s repeated a prior continuation token", ErrInvalidListing, prefix)
		}
		seenTokens[page.NextContinuationToken] = struct{}{}
		token = page.NextContinuationToken
	}
}

// decodeCompleteListPage is deliberately stricter than encoding/xml's normal
// struct decoding. Scalar S3 protocol fields must occur exactly once, so a
// malformed response cannot smuggle two IsTruncated values and let the last
// one silently turn an incomplete page into a complete listing.
func decodeCompleteListPage(data []byte) (listPage, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	root, err := nextCompleteListStart(decoder)
	if err != nil {
		return listPage{}, err
	}
	if root.Name.Local != "ListBucketResult" {
		return listPage{}, errors.New("unexpected list response root")
	}

	page := listPage{Contents: make([]ObjectInfo, 0)}
	truncatedSeen := false
	nextSeen := false
	for {
		token, err := decoder.Token()
		if err != nil {
			return listPage{}, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Space != root.Name.Space {
				if err := decoder.Skip(); err != nil {
					return listPage{}, err
				}
				continue
			}
			switch value.Name.Local {
			case "Contents":
				if len(page.Contents) >= maxCompleteListObjectsPerPage {
					return listPage{}, errors.New("list page exceeded requested object count")
				}
				object, err := decodeCompleteListObject(decoder, value, root.Name.Space)
				if err != nil {
					return listPage{}, err
				}
				page.Contents = append(page.Contents, object)
			case "IsTruncated":
				if truncatedSeen {
					return listPage{}, errors.New("duplicate IsTruncated field")
				}
				truncatedSeen = true
				raw, err := decodeCompleteListText(decoder, value, 5)
				if err != nil {
					return listPage{}, err
				}
				switch raw {
				case "true":
					page.IsTruncated = true
				case "false":
					page.IsTruncated = false
				default:
					return listPage{}, errors.New("invalid IsTruncated field")
				}
			case "NextContinuationToken":
				if nextSeen {
					return listPage{}, errors.New("duplicate NextContinuationToken field")
				}
				nextSeen = true
				page.NextContinuationToken, err = decodeCompleteListText(
					decoder, value, maxCompleteListTokenBytes)
				if err != nil {
					return listPage{}, err
				}
			default:
				if err := decoder.Skip(); err != nil {
					return listPage{}, err
				}
			}
		case xml.EndElement:
			if value.Name != root.Name {
				return listPage{}, errors.New("unexpected list response close")
			}
			if !truncatedSeen {
				return listPage{}, errors.New("missing IsTruncated field")
			}
			if page.IsTruncated && (!nextSeen ||
				!validCompleteListContinuationToken(page.NextContinuationToken)) {
				return listPage{}, errors.New("truncated page lacks a continuation token")
			}
			if !page.IsTruncated && nextSeen {
				return listPage{}, errors.New("complete page contains a continuation token")
			}
			if err := completeListOnlyTrailingSpace(decoder); err != nil {
				return listPage{}, err
			}
			return page, nil
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return listPage{}, errors.New("unexpected list response text")
			}
		}
	}
}

func validCompleteListContinuationToken(value string) bool {
	if value == "" || len(value) > maxCompleteListTokenBytes {
		return false
	}
	for index := range len(value) {
		if value[index] <= 0x20 || value[index] == 0x7f {
			return false
		}
	}
	return true
}

func nextCompleteListStart(decoder *xml.Decoder) (xml.StartElement, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return xml.StartElement{}, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			return value, nil
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return xml.StartElement{}, errors.New("unexpected text before list response")
			}
		}
	}
}

func decodeCompleteListObject(
	decoder *xml.Decoder,
	start xml.StartElement,
	namespace string,
) (ObjectInfo, error) {
	var object ObjectInfo
	keySeen, etagSeen, sizeSeen := false, false, false
	for {
		token, err := decoder.Token()
		if err != nil {
			return ObjectInfo{}, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Space != namespace {
				if err := decoder.Skip(); err != nil {
					return ObjectInfo{}, err
				}
				continue
			}
			switch value.Name.Local {
			case "Key":
				if keySeen {
					return ObjectInfo{}, errors.New("duplicate object Key field")
				}
				keySeen = true
				object.Key, err = decodeCompleteListText(
					decoder, value, maxCompleteListKeyBytes)
			case "ETag":
				if etagSeen {
					return ObjectInfo{}, errors.New("duplicate object ETag field")
				}
				etagSeen = true
				var raw string
				raw, err = decodeCompleteListText(
					decoder, value, maxCompleteListETagBytes+2)
				if err == nil {
					object.ETag, err = normalizeCompleteListETag(raw)
				}
			case "Size":
				if sizeSeen {
					return ObjectInfo{}, errors.New("duplicate object Size field")
				}
				sizeSeen = true
				var raw string
				raw, err = decodeCompleteListText(decoder, value, 20)
				if err == nil {
					object.Size, err = parseCompleteListSize(raw)
				}
			default:
				err = decoder.Skip()
			}
			if err != nil {
				return ObjectInfo{}, err
			}
		case xml.EndElement:
			if value.Name != start.Name {
				return ObjectInfo{}, errors.New("unexpected object metadata close")
			}
			if !keySeen || !etagSeen || !sizeSeen || object.Key == "" || object.ETag == "" {
				return ObjectInfo{}, errors.New("incomplete object metadata")
			}
			return object, nil
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return ObjectInfo{}, errors.New("unexpected object metadata text")
			}
		}
	}
}

func decodeCompleteListText(
	decoder *xml.Decoder,
	start xml.StartElement,
	maxBytes int,
) (string, error) {
	var value strings.Builder
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", err
		}
		switch part := token.(type) {
		case xml.CharData:
			if len(part) > maxBytes-value.Len() {
				return "", errors.New("list response field exceeded its length bound")
			}
			_, _ = value.Write(part)
		case xml.EndElement:
			if part.Name != start.Name {
				return "", errors.New("unexpected list response field close")
			}
			return value.String(), nil
		default:
			return "", errors.New("list response scalar contained nested content")
		}
	}
}

func normalizeCompleteListETag(value string) (string, error) {
	if len(value) < 3 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", errors.New("invalid object ETag")
	}
	value = value[1 : len(value)-1]
	if value == "" || len(value) > maxCompleteListETagBytes {
		return "", errors.New("invalid object ETag")
	}
	// RFC 9110 entity-tags are one quoted sequence of etagc bytes. Reject
	// whitespace, controls, DEL, and an embedded quote so callers receive one
	// unambiguous normalized identity rather than a permissively trimmed header.
	for index := range len(value) {
		if value[index] <= 0x20 || value[index] == '"' || value[index] == 0x7f {
			return "", errors.New("invalid object ETag")
		}
	}
	return value, nil
}

func parseCompleteListSize(value string) (int64, error) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, errors.New("invalid object Size")
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return 0, errors.New("invalid object Size")
		}
	}
	size, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, errors.New("invalid object Size")
	}
	return size, nil
}

func completeListOnlyTrailingSpace(decoder *xml.Decoder) error {
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if text, ok := token.(xml.CharData); !ok || strings.TrimSpace(string(text)) != "" {
			return errors.New("unexpected content after list response")
		}
	}
}

// s3Error is the XML error document S3-compatible services return; Code is
// the machine-readable truth the HTTP status alone hides (NoSuchKey vs
// NoSuchBucket are both 404s).
type s3Error struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// readS3Error parses the response body's error document (best effort; an
// unparseable body yields an empty Code, which callers treat as "not the code
// I was hoping for" — i.e., loud).
func readS3Error(resp *http.Response) s3Error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e s3Error
	_ = xml.Unmarshal(body, &e)
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(body))
	}
	return e
}

func httpError(op, key string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("blob: %s %s: %s: %s", op, key, resp.Status, strings.TrimSpace(string(body)))
}

// do builds, signs (SigV4), and sends one request. key "" targets the bucket
// itself (List). Path-style addressing: /<bucket>[/<key>].
func (c *Client) do(ctx context.Context, method, key string, query url.Values, body []byte, extraHeaders map[string]string) (*http.Response, error) {
	u := *c.base
	u.Path = "/" + c.cfg.Bucket
	if key != "" {
		u.Path += "/" + key
	}
	u.RawPath = uriEncodePath(u.Path)
	if query != nil {
		u.RawQuery = canonicalQuery(query)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("blob: build request: %w", err)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	c.sign(req, body)
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blob: %s %s: %w", strings.ToLower(method), key, err)
	}
	return resp, nil
}

// sign applies AWS Signature Version 4 (service "s3", single-chunk payload).
func (c *Client) sign(req *http.Request, body []byte) {
	now := c.cfg.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateScope := now.Format("20060102")
	payloadHash := sha256Hex(body)

	req.Header.Set("host", req.URL.Host)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	// Canonical headers: every header we set, lowercase, sorted.
	var names []string
	canon := map[string]string{}
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		canon[lk] = strings.TrimSpace(vs[0])
		names = append(names, lk)
	}
	// http.Request drops Host from Header on send but SigV4 requires it.
	if _, ok := canon["host"]; !ok {
		canon["host"] = req.URL.Host
		names = append(names, "host")
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, n := range names {
		ch.WriteString(n + ":" + canon[n] + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonicalURI := req.URL.RawPath
	if canonicalURI == "" {
		canonicalURI = uriEncodePath(req.URL.Path)
	}
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		req.URL.RawQuery,
		ch.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateScope + "/" + c.cfg.Region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	k := hmacSHA256([]byte("AWS4"+c.cfg.SecretKey), dateScope)
	k = hmacSHA256(k, c.cfg.Region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(k, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.cfg.AccessKey, scope, signedHeaders, signature))
	// Go's client sends Host from req.Host; keep header map clean.
	req.Host = req.URL.Host
	req.Header.Del("host")
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// uriEncodePath percent-encodes a path per the SigV4 rules: each segment
// encoded, '/' preserved, unreserved characters (A-Za-z0-9 - . _ ~) literal.
func uriEncodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

// canonicalQuery encodes query parameters per SigV4: keys sorted, every key
// and value uriEncoded, joined with & — this exact string is both sent and
// signed, so they can never disagree.
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range q[k] {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

func uriEncode(s string) string {
	var b strings.Builder
	for _, ch := range []byte(s) {
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9',
			ch == '-', ch == '.', ch == '_', ch == '~':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}
