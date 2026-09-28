package s3probe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
	"github.com/danilopopovikj/wassup/internal/probe/facet"
)

var clock = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func newBucket() *Bucket {
	return &Bucket{now: func() time.Time { return clock }, client: &http.Client{Timeout: 2 * time.Second}}
}

func cfg(endpoint string) config {
	return config{
		target: "media", bucket: "media", base: endpoint + "/media/", region: "fsn1",
		creds: credentials{accessKey: "AK", secretKey: "SK"}, accessEnv: "MEDIA_S3_KEY", secretEnv: "MEDIA_S3_SECRET",
		maxObjects: defaultMaxObjects,
		tick:       time.Second, interval: time.Minute, timeout: 2 * time.Second,
	}
}

// fakeStore serves HeadBucket and a paged ListObjectsV2 for bucket "media"
// with pages objects each, total objects in all; each object is 1000 bytes.
type fakeStore struct {
	objects, perPage int
	lists            atomic.Int32
	lastAuth         atomic.Value // string
	headStatus       int
}

func (f *fakeStore) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastAuth.Store(r.Header.Get("Authorization"))
		if !strings.HasPrefix(r.URL.Path, "/media/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodHead {
			status := f.headStatus
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			return
		}
		f.lists.Add(1)
		q := r.URL.Query()
		if q.Get("list-type") != "2" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		start := 0
		if tok := q.Get("continuation-token"); tok != "" {
			fmt.Sscanf(tok, "from-%d", &start)
		}
		end := min(start+f.perPage, f.objects)
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>media</Name>`)
		fmt.Fprintf(&b, "<KeyCount>%d</KeyCount>", end-start)
		if end < f.objects {
			fmt.Fprintf(&b, "<IsTruncated>true</IsTruncated><NextContinuationToken>from-%d</NextContinuationToken>", end)
		} else {
			b.WriteString("<IsTruncated>false</IsTruncated>")
		}
		for i := start; i < end; i++ {
			mod := clock.Add(-time.Duration(f.objects-i) * time.Minute)
			fmt.Fprintf(&b, "<Contents><Key>o/%d</Key><LastModified>%s</LastModified><Size>1000</Size></Contents>", i, mod.Format("2006-01-02T15:04:05.000Z"))
		}
		b.WriteString("</ListBucketResult>")
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(b.String()))
	})
}

func TestBucketSizeAndObjects(t *testing.T) {
	f := &fakeStore{objects: 2500, perPage: 1000}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := newBucket()
	c := cfg(srv.URL)
	c.listObjects = true
	c.quota = facet.N(10_000_000)
	o := p.poll(context.Background(), c)
	if o.Err != "" {
		t.Fatalf("err = %q", o.Err)
	}
	if o.Metrics["objects"] != 2500 || o.Metrics["used_bytes"] != 2_500_000 {
		t.Fatalf("metrics = %v", o.Metrics)
	}
	if o.Metrics["total_bytes"] != 10_000_000 || o.Metrics["disk_pct"] != 25 {
		t.Fatalf("quota metrics = %v", o.Metrics)
	}
	if _, ok := o.Metrics["latency_ms"]; !ok {
		t.Fatalf("latency missing: %v", o.Metrics)
	}
	if o.Detail["last_write"] != clock.Add(-time.Minute).Format(time.RFC3339) {
		t.Fatalf("last_write = %v", o.Detail["last_write"])
	}
	if f.lists.Load() != 3 {
		t.Fatalf("pages fetched = %d, want 3", f.lists.Load())
	}
	if len(o.Conditions) != 0 {
		t.Fatalf("conditions = %+v", o.Conditions)
	}
	auth, _ := f.lastAuth.Load().(string)
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AK/20260928/fsn1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=") {
		t.Fatalf("authorization = %q", auth)
	}
	if p.Health().State != probe.HealthOK {
		t.Fatalf("health = %+v", p.Health())
	}
}

func TestBucketListingCapped(t *testing.T) {
	f := &fakeStore{objects: 5000, perPage: 1000}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := newBucket()
	c := cfg(srv.URL)
	c.listObjects = true
	c.maxObjects = 2000
	o := p.poll(context.Background(), c)
	if o.Err != "" {
		t.Fatalf("err = %q", o.Err)
	}
	if _, ok := o.Metrics["used_bytes"]; ok {
		t.Fatalf("a partial sum must not be reported as the size: %v", o.Metrics)
	}
	if o.Detail["objects_at_least"] != 2000 || o.Detail["used_bytes_at_least"] != int64(2_000_000) {
		t.Fatalf("detail = %v", o.Detail)
	}
	if f.lists.Load() != 2 {
		t.Fatalf("pages fetched = %d, want 2", f.lists.Load())
	}
	want := "bucket listing skipped, more than 2000 objects: set prefix to size a part of the bucket, or raise max_objects, at one request per 1000 objects a round"
	if h := p.Health(); h.State != probe.HealthDegraded || h.Message != want {
		t.Fatalf("health = %+v", h)
	}
	if o.Detail["size"] != want {
		t.Fatalf("detail = %v", o.Detail)
	}
}

// The default is the cheap check: one HeadBucket, no listing. What was not
// read is not a number, and the detail says how to read it and what it
// costs.
func TestBucketIsNotListedByDefault(t *testing.T) {
	f := &fakeStore{objects: 2500, perPage: 1000}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := newBucket()
	c := cfg(srv.URL)
	c.quota = facet.N(10_000_000)
	c.prefix = "uploads/"
	o := p.poll(context.Background(), c)
	if o.Err != "" {
		t.Fatalf("err = %q", o.Err)
	}
	if f.lists.Load() != 0 {
		t.Fatalf("listed %d pages without being asked to", f.lists.Load())
	}
	for _, k := range []string{"used_bytes", "objects", "disk_pct"} {
		if v, ok := o.Metrics[k]; ok {
			t.Errorf("%s was not read and must not be a number, got %v", k, v)
		}
	}
	if _, ok := o.Metrics["latency_ms"]; !ok {
		t.Errorf("latency missing: %v", o.Metrics)
	}
	if o.Detail["size"] != "object count not read; set list_objects: true to size the bucket, which lists every object under the prefix (one request per 1000 objects)" {
		t.Errorf("size = %v", o.Detail["size"])
	}
	if o.Detail["ignored"] != "without list_objects: true these settings do nothing: prefix, quota_bytes" {
		t.Errorf("ignored = %v", o.Detail["ignored"])
	}
	if _, ok := o.Detail["last_write"]; ok {
		t.Errorf("last_write comes from the listing: %v", o.Detail)
	}
	if len(o.Conditions) != 0 || p.Health().State != probe.HealthOK {
		t.Errorf("conditions = %+v health = %+v", o.Conditions, p.Health())
	}
}

func TestListObjectsIsOptIn(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "a")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "b")
	c, err := parse(map[string]any{"bucket": "media"})
	if err != nil || c.listObjects {
		t.Fatalf("default: %+v err=%v", c, err)
	}
	c, err = parse(map[string]any{"bucket": "media", "list_objects": true, "access_key_env": "AWS_ACCESS_KEY_ID"})
	if err != nil || !c.listObjects || c.accessEnv != "AWS_ACCESS_KEY_ID" || c.secretEnv != "AWS_SECRET_ACCESS_KEY" {
		t.Fatalf("opted in: %+v err=%v", c, err)
	}
	if (&Bucket{}).Validate(map[string]any{"bucket": "media", "list_objects": "yes"}) == nil {
		t.Fatal("list_objects is true or false")
	}
}

// A key that may check the bucket but not list it: the bucket is there, the
// size is not known, and the message says which permission is missing.
func TestBucketListingDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return
		}
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`))
	}))
	defer srv.Close()

	p := newBucket()
	c := cfg(srv.URL)
	c.listObjects = true
	o := p.poll(context.Background(), c)
	if o.Err != "" {
		t.Fatalf("the bucket answered: err = %q", o.Err)
	}
	if _, ok := o.Metrics["used_bytes"]; ok {
		t.Fatalf("metrics = %v", o.Metrics)
	}
	want := "the bucket answers, the listing was denied: the keys in MEDIA_S3_KEY and MEDIA_S3_SECRET are allowed to check the bucket but not to list it; " +
		"allow s3:ListBucket, or take list_objects out (AccessDenied: Access Denied)"
	if h := p.Health(); h.State != probe.HealthDegraded || h.Message != want {
		t.Fatalf("health = %+v", h)
	}
	if o.Detail["list_error"] != want {
		t.Fatalf("detail = %v", o.Detail)
	}
}

// A refused connection is reported in the first round, at once.
func TestRefusedConnectionIsReportedAtOnce(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	dead := srv.URL
	srv.Close()
	t.Setenv("AWS_ACCESS_KEY_ID", "a")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "b")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan probe.Observation, 4)
	p := &Bucket{}
	start := time.Now()
	if err := p.Start(ctx, map[string]any{"bucket": "media", "endpoint": dead, "list_objects": true, "_target": "media"}, out); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-out:
		if took := time.Since(start); took > time.Second {
			t.Errorf("the refusal took %s", took)
		}
		host := strings.TrimPrefix(dead, "http://")
		if !strings.HasPrefix(o.Err, "connection refused on "+host+": nothing listens there. Check the endpoint and its port (HEAD "+dead+"/media/: ") {
			t.Errorf("err = %q", o.Err)
		}
		if o.Metrics != nil {
			t.Errorf("a failed read has no numbers: %v", o.Metrics)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no observation")
	}
}

func TestBucketMissing(t *testing.T) {
	f := &fakeStore{headStatus: http.StatusNotFound}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := newBucket()
	missing := cfg(srv.URL)
	missing.listObjects = true
	o := p.poll(context.Background(), missing)
	if o.Err != "" {
		t.Fatalf("err = %q", o.Err)
	}
	cnd, ok := model.HasCondition(o.Conditions, model.CondNotReady)
	if !ok || cnd.Ref != "media" || !cnd.Since.Equal(clock) || cnd.Detail != "bucket media does not exist" {
		t.Fatalf("conditions = %+v", o.Conditions)
	}
	if _, ok := o.Metrics["used_bytes"]; ok {
		t.Fatalf("no listing without a bucket: %v", o.Metrics)
	}
	if h := p.Health(); !strings.HasSuffix(h.Message, "check the name of the bucket, the endpoint and the region") {
		t.Fatalf("health = %+v", h)
	}
	if f.lists.Load() != 0 {
		t.Fatal("listed a bucket that does not exist")
	}
	// Since is stable while the bucket is still missing.
	p.now = func() time.Time { return clock.Add(time.Minute) }
	o = p.poll(context.Background(), cfg(srv.URL))
	cnd, _ = model.HasCondition(o.Conditions, model.CondNotReady)
	if !cnd.Since.Equal(clock) {
		t.Fatalf("second poll since = %v", cnd.Since)
	}
}

func TestBucketAccessDenied(t *testing.T) {
	f := &fakeStore{headStatus: http.StatusForbidden}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	p := newBucket()
	o := p.poll(context.Background(), cfg(srv.URL))
	if o.Err == "" || o.Metrics != nil || len(o.Conditions) != 0 {
		t.Fatalf("err=%q metrics=%v conditions=%v", o.Err, o.Metrics, o.Conditions)
	}
	want := "access denied to bucket media: check the keys in MEDIA_S3_KEY and MEDIA_S3_SECRET, and that they are allowed s3:ListBucket on the bucket (HEAD " + srv.URL + "/media/: HTTP 403)"
	if o.Err != want {
		t.Fatalf("err = %q", o.Err)
	}
	if p.Health().State != probe.HealthFailed {
		t.Fatalf("health = %+v", p.Health())
	}
}

func TestBucketConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	dead := srv.URL
	srv.Close()

	p := newBucket()
	o := p.poll(context.Background(), cfg(dead))
	if o.Err == "" || o.Metrics != nil {
		t.Fatalf("err=%q metrics=%v", o.Err, o.Metrics)
	}
	cnd, ok := model.HasCondition(o.Conditions, model.CondConnectionRefused)
	if !ok || cnd.Ref != "media" || !cnd.Since.Equal(clock) {
		t.Fatalf("conditions = %+v", o.Conditions)
	}
	if p.Health().State != probe.HealthDegraded {
		t.Fatalf("health = %+v", p.Health())
	}
}

func TestParseAddressing(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "a")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "b")
	t.Setenv("HETZNER_S3_KEY", "hk")
	t.Setenv("HETZNER_S3_SECRET", "hs")

	c, err := parse(map[string]any{"bucket": "assets", "region": "eu-central-1"})
	if err != nil || c.base != "https://assets.s3.eu-central-1.amazonaws.com/" {
		t.Fatalf("aws: base=%q err=%v", c.base, err)
	}
	c, err = parse(map[string]any{"bucket": "media", "endpoint": "https://fsn1.your-objectstorage.com/", "region": "fsn1",
		"access_key_env": "HETZNER_S3_KEY", "secret_key_env": "HETZNER_S3_SECRET", "quota_bytes": 5000, "max_objects": 100})
	if err != nil || c.base != "https://fsn1.your-objectstorage.com/media/" || c.creds.accessKey != "hk" || !c.quota.Set || c.maxObjects != 100 {
		t.Fatalf("path style: %+v err=%v", c, err)
	}
	c, err = parse(map[string]any{"bucket": "media", "endpoint": "https://nyc3.digitaloceanspaces.com", "path_style": false})
	if err != nil || c.base != "https://media.nyc3.digitaloceanspaces.com/" {
		t.Fatalf("virtual host: base=%q err=%v", c.base, err)
	}
	if _, err := parse(map[string]any{"bucket": "x", "secret_key_env": "NOPE_NOT_SET"}); err == nil || !strings.Contains(err.Error(), "NOPE_NOT_SET") {
		t.Fatalf("missing secret env: %v", err)
	}
}

func TestValidate(t *testing.T) {
	p := &Bucket{}
	bad := []map[string]any{
		{},
		{"bucket": "b", "endpoint": "fsn1.your-objectstorage.com"},
		{"bucket": "b", "interval": "soon"},
		{"bucket": "b", "path_style": "yes"},
		{"bucket": "b", "quota_bytes": -1},
		{"bucket": "b", "max_objects": "many"},
	}
	for _, spec := range bad {
		if p.Validate(spec) == nil {
			t.Fatalf("spec %v should not validate", spec)
		}
	}
	if err := p.Validate(map[string]any{"bucket": "b", "endpoint": "http://minio:9000", "prefix": "uploads/", "quota_bytes": 1e12}); err != nil {
		t.Fatal(err)
	}
}
