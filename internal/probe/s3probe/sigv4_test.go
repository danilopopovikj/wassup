package s3probe

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSignKnownAnswer is the "GET Bucket (List Objects)" example from the
// AWS Signature Version 4 documentation: the signature must match theirs.
func TestSignKnownAnswer(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", nil)
	c := credentials{accessKey: "AKIAIOSFODNN7EXAMPLE", secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	sign(req, c, "us-east-1", time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))

	got := req.Header.Get("Authorization")
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date, " +
		"Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"
	if got != want {
		t.Fatalf("authorization\n got %s\nwant %s", got, want)
	}
	if req.Header.Get("x-amz-date") != "20130524T000000Z" {
		t.Fatalf("x-amz-date = %q", req.Header.Get("x-amz-date"))
	}
}

func TestSignSessionToken(t *testing.T) {
	req, _ := http.NewRequest(http.MethodHead, "http://127.0.0.1:9000/my-bucket/", nil)
	sign(req, credentials{accessKey: "a", secretKey: "b", sessionToken: "tok"}, "us-east-1", time.Now())
	if req.Header.Get("x-amz-security-token") != "tok" {
		t.Fatal("session token header missing")
	}
	if !strings.Contains(req.Header.Get("Authorization"), "SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token,") {
		t.Fatalf("signed headers: %s", req.Header.Get("Authorization"))
	}
}

func TestCanonicalQueryEncoding(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://h/b/?list-type=2&prefix=a+b%2Fc&continuation-token=x%3D%3D", nil)
	if got := canonicalQuery(req.URL.Query()); got != "continuation-token=x%3D%3D&list-type=2&prefix=a%20b%2Fc" {
		t.Fatalf("canonical query = %s", got)
	}
	if got := canonicalPath(req.URL); got != "/b/" {
		t.Fatalf("canonical path = %s", got)
	}
}
