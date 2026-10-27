package storage

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// fakeS3 is a minimal in-memory stand-in for the S3 REST API, covering just
// the operations storage.S3 uses (Head/Put/Get/Delete/ListObjectsV2), with
// path-style addressing. It's enough to drive the real aws-sdk-go-v2 client
// end to end without a real AWS account or network access — the client
// only needs S3Options.Endpoint pointed at this server.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte // key -> body, keyed without the leading slash
	modTime map[string]time.Time
}

// newFakeS3 serves over TLS (with an httptest self-signed cert) rather than
// plain HTTP: aws-sdk-go-v2 streams S3 PutObject bodies (ours is an
// unseekable io.Reader — the incoming request body) as an aws-chunked
// upload with a trailing checksum, and refuses to do that without TLS
// (see aws/aws-sdk-go-v2#2673-style errors). Real S3/R2 endpoints are
// always TLS, so this matches production, not just a test convenience.
func newFakeS3(t *testing.T) (*httptest.Server, *fakeS3) {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}, modTime: map[string]time.Time{}}
	srv := httptest.NewTLSServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return srv, f
}

// handle expects path-style requests: /{bucket}/{key...} or /{bucket}? for
// bucket-level operations (ListObjectsV2).
func (f *fakeS3) handle(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(parts) < 1 || parts[0] == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	key := ""
	if len(parts) == 2 {
		key = parts[1]
	}

	if key == "" && r.URL.Query().Get("list-type") == "2" {
		f.listObjectsV2(w, r)
		return
	}

	switch r.Method {
	case http.MethodHead:
		f.headObject(w, key)
	case http.MethodPut:
		f.putObject(w, r, key)
	case http.MethodGet:
		f.getObject(w, key)
	case http.MethodDelete:
		f.deleteObject(w, key)
	default:
		http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) headObject(w http.ResponseWriter, key string) {
	f.mu.Lock()
	body, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound) // no body: SDK maps this to ErrorCode "NotFound"
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) putObject(w http.ResponseWriter, r *http.Request, key string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusInternalServerError)
		return
	}
	f.mu.Lock()
	f.objects[key] = body
	f.modTime[key] = time.Now().UTC()
	f.mu.Unlock()
	w.Header().Set("ETag", `"fake-etag"`)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) getObject(w http.ResponseWriter, key string) {
	f.mu.Lock()
	body, ok := f.objects[key]
	f.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message><Key>%s</Key><RequestId>fake</RequestId></Error>`, key)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (f *fakeS3) deleteObject(w http.ResponseWriter, key string) {
	f.mu.Lock()
	delete(f.objects, key)
	delete(f.modTime, key)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

type xmlListContents struct {
	Key          string    `xml:"Key"`
	LastModified time.Time `xml:"LastModified"`
	Size         int64     `xml:"Size"`
}

type xmlListResult struct {
	XMLName               xml.Name          `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
	Name                  string            `xml:"Name"`
	Prefix                string            `xml:"Prefix"`
	MaxKeys               int               `xml:"MaxKeys"`
	IsTruncated           bool              `xml:"IsTruncated"`
	NextContinuationToken string            `xml:"NextContinuationToken,omitempty"`
	Contents              []xmlListContents `xml:"Contents"`
}

func (f *fakeS3) listObjectsV2(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	cursor := r.URL.Query().Get("continuation-token")
	maxKeys := 1000
	if v := r.URL.Query().Get("max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxKeys = n
		}
	}

	f.mu.Lock()
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	start := 0
	if cursor != "" {
		start = sort.SearchStrings(keys, cursor) + 1
	}
	end := start + maxKeys
	if end > len(keys) {
		end = len(keys)
	}
	if start > len(keys) {
		start = len(keys)
	}

	result := xmlListResult{Prefix: prefix, MaxKeys: maxKeys}
	for _, k := range keys[start:end] {
		result.Contents = append(result.Contents, xmlListContents{
			Key: k, LastModified: f.modTime[k], Size: int64(len(f.objects[k])),
		})
	}
	if end < len(keys) {
		result.IsTruncated = true
		result.NextContinuationToken = keys[end-1]
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(result)
}

// newTestS3 builds a *S3 by hand (this test file is in package storage, so
// the unexported fields are reachable) rather than through the exported
// NewS3, so it can inject an HTTP client that trusts the fake server's
// self-signed cert — NewS3 itself is covered separately by
// TestNewS3WithStaticCredentials / TestNewS3WithoutStaticCredentials.
func newTestS3(t *testing.T, prefix string) *S3 {
	t.Helper()
	srv, _ := newFakeS3(t)

	insecureClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only fake server
	}}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithHTTPClient(insecureClient),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("fake-access-key", "fake-secret-key", ""),
		),
	)
	if err != nil {
		t.Fatalf("LoadDefaultConfig: %v", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.UsePathStyle = true
		// Skip the SDK's default aws-chunked + trailing-checksum PUT
		// encoding: our fake server stores the request body verbatim
		// rather than parsing that framing, so it needs the plain payload.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	return &S3{client: client, bucket: "test-bucket", prefix: prefix}
}

func TestS3ExistsPutGetDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestS3(t, "")

	if exists, err := s.Exists(ctx, "hash1"); err != nil || exists {
		t.Fatalf("Exists before put = (%v, %v), want (false, nil)", exists, err)
	}

	payload := []byte("hello s3")
	if err := s.Put(ctx, "hash1", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if exists, err := s.Exists(ctx, "hash1"); err != nil || !exists {
		t.Fatalf("Exists after put = (%v, %v), want (true, nil)", exists, err)
	}

	r, size, err := s.Get(ctx, "hash1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = r.Close() }()
	if size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", size, len(payload))
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}

	if err := s.Delete(ctx, "hash1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if exists, _ := s.Exists(ctx, "hash1"); exists {
		t.Fatal("entry still exists after Delete")
	}
}

func TestS3PutDuplicateReturnsAlreadyExists(t *testing.T) {
	ctx := context.Background()
	s := newTestS3(t, "")
	if err := s.Put(ctx, "hash1", bytes.NewReader([]byte("a")), 1); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	err := s.Put(ctx, "hash1", bytes.NewReader([]byte("b")), 1)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second Put error = %v, want ErrAlreadyExists", err)
	}
}

func TestS3GetMissingReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestS3(t, "")
	_, _, err := s.Get(ctx, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

func TestS3DeleteMissingReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestS3(t, "")
	if err := s.Delete(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete error = %v, want ErrNotFound", err)
	}
}

func TestS3ListWithPrefixAndPagination(t *testing.T) {
	ctx := context.Background()
	s := newTestS3(t, "myprefix")

	hashes := []string{"a1", "a2", "a3"}
	for _, h := range hashes {
		if err := s.Put(ctx, h, bytes.NewReader([]byte("x")), 1); err != nil {
			t.Fatalf("Put(%s): %v", h, err)
		}
	}

	page1, err := s.List(ctx, "", "", 2)
	if err != nil {
		t.Fatalf("List page1: %v", err)
	}
	if len(page1.Entries) != 2 {
		t.Fatalf("page1 entries = %d, want 2", len(page1.Entries))
	}
	// unkey should strip "myprefix/" back off, so hashes come back bare.
	for _, e := range page1.Entries {
		if strings.Contains(e.Key, "myprefix") {
			t.Fatalf("entry hash %q still has the prefix", e.Key)
		}
	}
	if page1.NextCursor == "" {
		t.Fatal("expected a NextCursor for a truncated page")
	}

	page2, err := s.List(ctx, "", page1.NextCursor, 2)
	if err != nil {
		t.Fatalf("List page2: %v", err)
	}
	if len(page2.Entries) != 1 {
		t.Fatalf("page2 entries = %d, want 1", len(page2.Entries))
	}
	if page2.NextCursor != "" {
		t.Fatalf("page2.NextCursor = %q, want empty (last page)", page2.NextCursor)
	}
}

func TestS3ListWithoutPrefix(t *testing.T) {
	ctx := context.Background()
	s := newTestS3(t, "")
	if err := s.Put(ctx, "onlyhash", bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	page, err := s.List(ctx, "", "", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Key != "onlyhash" {
		t.Fatalf("List = %+v, want one entry for onlyhash", page.Entries)
	}
}

// newBrokenTestS3 returns a *S3 whose every request gets a 500, to
// exercise Exists/Put/Get/Delete/List's generic (non-404) error branches.
func newBrokenTestS3(t *testing.T) *S3 {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		if r.Method != http.MethodHead {
			_, _ = fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InternalError</Code><Message>fake failure</Message><RequestId>fake</RequestId></Error>`)
		}
	}))
	t.Cleanup(srv.Close)

	insecureClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only fake server
	}}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithHTTPClient(insecureClient),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("fake-access-key", "fake-secret-key", ""),
		),
	)
	if err != nil {
		t.Fatalf("LoadDefaultConfig: %v", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.UsePathStyle = true
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.RetryMaxAttempts = 1 // don't waste test time retrying a deterministic 500
	})
	return &S3{client: client, bucket: "test-bucket"}
}

func TestS3ExistsSurfacesUnexpectedError(t *testing.T) {
	s := newBrokenTestS3(t)
	if _, err := s.Exists(context.Background(), "hash1"); err == nil {
		t.Fatal("expected an error from a 500 response")
	}
}

func TestS3PutSurfacesUnexpectedError(t *testing.T) {
	s := newBrokenTestS3(t)
	err := s.Put(context.Background(), "hash1", bytes.NewReader([]byte("x")), 1)
	if err == nil || errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Put error = %v, want a generic (non-ErrAlreadyExists) error", err)
	}
}

func TestS3GetSurfacesUnexpectedError(t *testing.T) {
	s := newBrokenTestS3(t)
	_, _, err := s.Get(context.Background(), "hash1")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want a generic (non-ErrNotFound) error", err)
	}
}

func TestS3DeleteSurfacesUnexpectedError(t *testing.T) {
	// Delete's own Exists pre-check hits the broken server first, so this
	// exercises Delete's propagation of that error, not DeleteObject itself.
	s := newBrokenTestS3(t)
	err := s.Delete(context.Background(), "hash1")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete error = %v, want a generic (non-ErrNotFound) error", err)
	}
}

// newPartlyBrokenTestS3 answers HEAD requests as if headExists reports,
// and fails every other method with a 500 — for exercising PutObject's and
// DeleteObject's own error branches specifically, as opposed to the
// earlier Exists() pre-check that both methods also make.
func newPartlyBrokenTestS3(t *testing.T, headExists bool) *S3 {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			if headExists {
				w.Header().Set("Content-Length", "1")
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InternalError</Code><Message>fake failure</Message><RequestId>fake</RequestId></Error>`)
	}))
	t.Cleanup(srv.Close)

	insecureClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only fake server
	}}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithHTTPClient(insecureClient),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("fake-access-key", "fake-secret-key", ""),
		),
	)
	if err != nil {
		t.Fatalf("LoadDefaultConfig: %v", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.UsePathStyle = true
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.RetryMaxAttempts = 1
	})
	return &S3{client: client, bucket: "test-bucket"}
}

func TestS3PutObjectCallSurfacesUnexpectedError(t *testing.T) {
	// HEAD says "doesn't exist" (so Put proceeds past its Exists check),
	// but the PUT itself 500s.
	s := newPartlyBrokenTestS3(t, false)
	err := s.Put(context.Background(), "hash1", bytes.NewReader([]byte("x")), 1)
	if err == nil || errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Put error = %v, want a generic error from PutObject itself", err)
	}
}

func TestS3DeleteObjectCallSurfacesUnexpectedError(t *testing.T) {
	// HEAD says "exists" (so Delete proceeds past its Exists check), but
	// the DELETE itself 500s.
	s := newPartlyBrokenTestS3(t, true)
	err := s.Delete(context.Background(), "hash1")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete error = %v, want a generic error from DeleteObject itself", err)
	}
}

func TestS3ListSurfacesUnexpectedError(t *testing.T) {
	s := newBrokenTestS3(t)
	if _, err := s.List(context.Background(), "", "", 10); err == nil {
		t.Fatal("expected an error from a 500 response")
	}
}

func TestNewS3WithStaticCredentials(t *testing.T) {
	_, err := NewS3(context.Background(), S3Options{
		Bucket:          "test-bucket",
		Region:          "us-east-1",
		Endpoint:        "https://example.invalid",
		UsePathStyle:    true,
		AccessKeyID:     "fake-access-key",
		SecretAccessKey: "fake-secret-key",
	})
	if err != nil {
		t.Fatalf("NewS3 with static credentials: %v", err)
	}
}

func TestNewS3WithoutStaticCredentials(t *testing.T) {
	// Leaving AccessKeyID/SecretAccessKey empty falls back to the SDK's
	// default credential chain — construction itself must still succeed
	// (it doesn't validate credentials eagerly, so no network call happens
	// here and no real server is needed).
	_, err := NewS3(context.Background(), S3Options{
		Bucket:       "test-bucket",
		Region:       "us-east-1",
		Endpoint:     "https://example.invalid",
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3 without static credentials: %v", err)
	}
}

func TestIsNotFound(t *testing.T) {
	if isNotFound(nil) {
		t.Error("isNotFound(nil) = true, want false")
	}
	if isNotFound(errors.New("some other error")) {
		t.Error("isNotFound(generic error) = true, want false")
	}
	if isNotFound(&smithy.GenericAPIError{Code: "AccessDenied"}) {
		t.Error("isNotFound(AccessDenied) = true, want false")
	}
	if !isNotFound(&smithy.GenericAPIError{Code: "NotFound"}) {
		t.Error("isNotFound(NotFound) = false, want true")
	}
	if !isNotFound(&smithy.GenericAPIError{Code: "NoSuchKey"}) {
		t.Error("isNotFound(NoSuchKey) = false, want true")
	}
}

func TestS3ListByKeyPrefix(t *testing.T) {
	t.Run("no bucket prefix", func(t *testing.T) { testListByKeyPrefix(t, newTestS3(t, "")) })
	t.Run("bucket prefix", func(t *testing.T) { testListByKeyPrefix(t, newTestS3(t, "myprefix")) })
}
