package awssig

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func fixedSigner(service string) *Signer {
	return &Signer{
		Region:  "us-east-1",
		Service: service,
		Now: func() time.Time {
			return time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
		},
	}
}

var testCreds = Credentials{
	AccessKeyID:     "AKIDEXAMPLE",
	SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
}

// The signing-key derivation chain is the part of SigV4 that is easiest to
// get subtly wrong and hardest to debug from an opaque 403, so its
// properties are pinned: it is deterministic, and it is bound to all four of
// secret, date, region and service. A key that ignored any one of those
// would still produce plausible-looking signatures that AWS rejects.
func TestSigningKeyIsDeterministicAndFullyScoped(t *testing.T) {
	base := signingKey("secret", "20150830", "us-east-1", "s3")

	if len(base) != sha256.Size {
		t.Fatalf("expected a %d-byte key, got %d", sha256.Size, len(base))
	}
	if !bytes.Equal(base, signingKey("secret", "20150830", "us-east-1", "s3")) {
		t.Fatal("signing key derivation must be deterministic")
	}

	variants := map[string][]byte{
		"secret":  signingKey("other", "20150830", "us-east-1", "s3"),
		"date":    signingKey("secret", "20150831", "us-east-1", "s3"),
		"region":  signingKey("secret", "20150830", "ap-south-1", "s3"),
		"service": signingKey("secret", "20150830", "us-east-1", "sqs"),
	}
	for changed, key := range variants {
		if bytes.Equal(base, key) {
			t.Errorf("signing key must change when the %s changes", changed)
		}
	}
}

func TestUriEncodeFollowsAWSRules(t *testing.T) {
	cases := []struct {
		in          string
		encodeSlash bool
		want        string
	}{
		{"simple", true, "simple"},
		{"a b", true, "a%20b"},                 // never "+"
		{"a/b", false, "a/b"},                  // path separators preserved
		{"a/b", true, "a%2Fb"},                 // encoded in query values
		{"~-_.", true, "~-_."},                 // unreserved, untouched
		{"tenant=1&x", true, "tenant%3D1%26x"}, // delimiters escaped
		{"caf\u00e9", true, "caf%C3%A9"},       // UTF-8, uppercase hex
	}
	for _, c := range cases {
		if got := uriEncode(c.in, c.encodeSlash); got != c.want {
			t.Errorf("uriEncode(%q, %v) = %q, want %q", c.in, c.encodeSlash, got, c.want)
		}
	}
}

func TestCanonicalQuerySortsAndEncodes(t *testing.T) {
	q := url.Values{}
	q.Set("prefix", "usage exports/")
	q.Set("X-Amz-Date", "20150830T123600Z")
	q.Set("a", "2")
	q.Add("a", "1")

	got := canonicalQuery(q)
	want := "X-Amz-Date=20150830T123600Z&a=1&a=2&prefix=usage%20exports%2F"
	if got != want {
		t.Fatalf("canonicalQuery()\n got: %s\nwant: %s", got, want)
	}
}

func TestSignRequestSetsSigV4Headers(t *testing.T) {
	req, err := http.NewRequest(http.MethodPut, "https://my-bucket.s3.ap-south-1.amazonaws.com/usage-exports/tenant-a/report.csv", strings.NewReader("a,b\n"))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "text/csv")

	signer := fixedSigner("s3")
	if err := signer.SignRequest(req, testCreds, HashPayload([]byte("a,b\n"))); err != nil {
		t.Fatalf("SignRequest() error = %v", err)
	}

	auth := req.Header.Get("Authorization")
	for _, want := range []string{
		"AWS4-HMAC-SHA256",
		"Credential=AKIDEXAMPLE/20150830/us-east-1/s3/aws4_request",
		"SignedHeaders=",
		"Signature=",
	} {
		if !strings.Contains(auth, want) {
			t.Errorf("Authorization header missing %q, got %q", want, auth)
		}
	}
	// content-type, host and the x-amz-* headers must all be covered.
	for _, want := range []string{"content-type", "host", "x-amz-content-sha256", "x-amz-date"} {
		if !strings.Contains(auth, want) {
			t.Errorf("expected %q to be a signed header, got %q", want, auth)
		}
	}
	if req.Header.Get("X-Amz-Date") != "20150830T123600Z" {
		t.Errorf("unexpected X-Amz-Date %q", req.Header.Get("X-Amz-Date"))
	}
}

func TestSignRequestIncludesSessionToken(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://sqs.ap-south-1.amazonaws.com/", nil)
	creds := testCreds
	creds.SessionToken = "session-token-value"

	if err := fixedSigner("sqs").SignRequest(req, creds, EmptyPayloadHash); err != nil {
		t.Fatalf("SignRequest() error = %v", err)
	}
	if req.Header.Get("X-Amz-Security-Token") != "session-token-value" {
		t.Error("expected temporary credentials to add X-Amz-Security-Token")
	}
	if !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Error("expected the session token header to be signed, not just sent")
	}
}

func TestPresignGETProducesAnExpiringURL(t *testing.T) {
	signer := fixedSigner("s3")

	got, err := signer.PresignGET(
		"https://my-bucket.s3.ap-south-1.amazonaws.com/usage-exports/tenant-a/report.csv",
		testCreds, 15*time.Minute,
	)
	if err != nil {
		t.Fatalf("PresignGET() error = %v", err)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("presigned URL does not parse: %v", err)
	}
	q := u.Query()

	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		t.Errorf("unexpected algorithm %q", q.Get("X-Amz-Algorithm"))
	}
	if q.Get("X-Amz-Expires") != "900" {
		t.Errorf("expected a 900 second expiry, got %q", q.Get("X-Amz-Expires"))
	}
	if q.Get("X-Amz-SignedHeaders") != "host" {
		t.Errorf("a browser-usable URL must sign only host, got %q", q.Get("X-Amz-SignedHeaders"))
	}
	if len(q.Get("X-Amz-Signature")) != 64 {
		t.Errorf("expected a 64-char hex signature, got %q", q.Get("X-Amz-Signature"))
	}
	// No credential material beyond the access key id may appear in a URL
	// that gets emailed around and logged by proxies.
	if strings.Contains(got, testCreds.SecretAccessKey) {
		t.Fatal("the secret access key leaked into the presigned URL")
	}
}

func TestPresignGETCapsLifetimeAtSevenDays(t *testing.T) {
	got, err := fixedSigner("s3").PresignGET(
		"https://my-bucket.s3.amazonaws.com/x.csv", testCreds, 30*24*time.Hour,
	)
	if err != nil {
		t.Fatalf("PresignGET() error = %v", err)
	}
	u, _ := url.Parse(got)
	if u.Query().Get("X-Amz-Expires") != "604800" {
		t.Errorf("expected the SigV4 seven-day cap, got %q", u.Query().Get("X-Amz-Expires"))
	}
}

func TestSignRequestRejectsMissingCredentials(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://s3.amazonaws.com/", nil)
	if err := fixedSigner("s3").SignRequest(req, Credentials{}, EmptyPayloadHash); err == nil {
		t.Fatal("expected signing to fail without credentials rather than send an unsigned request")
	}
}
