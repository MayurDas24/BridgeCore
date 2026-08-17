// Package awssig implements AWS Signature Version 4 request signing and
// URL presigning over the standard library only.
//
// BridgeCore talks to exactly three AWS APIs from the application process —
// S3 PutObject/GetObject, SQS SendMessage, and Secrets Manager
// GetSecretValue — and each is a single, well-specified HTTPS call. Pulling
// in the full AWS SDK for that would add well over a hundred transitive
// modules to a build that otherwise has six, for three requests. Signing
// them directly keeps the dependency graph auditable, keeps container images
// small, and makes the exact bytes on the wire visible in code rather than
// buried in a generated client.
//
// The tradeoff is deliberate and bounded: this package implements the two
// SigV4 flows BridgeCore needs (header-signed requests and query-presigned
// GETs), not the whole surface of AWS request signing. A deployment that
// grew to use many more AWS services should adopt aws-sdk-go-v2 and delete
// this package — the ObjectStore and Notifier interfaces in internal/exports
// exist so that swap touches nothing above the adapter.
package awssig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	algorithm       = "AWS4-HMAC-SHA256"
	terminator      = "aws4_request"
	amzDateFormat   = "20060102T150405Z"
	shortDateFormat = "20060102"

	// UnsignedPayload is the payload-hash sentinel used for presigned URLs,
	// where the body is not known at signing time.
	UnsignedPayload = "UNSIGNED-PAYLOAD"

	// EmptyPayloadHash is the SHA-256 of an empty body, required on requests
	// that legitimately have none.
	EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// Credentials are the AWS credentials used to sign a request. Token is set
// for temporary credentials (an ECS task role, or any STS-issued session),
// which is the only kind BridgeCore uses in production.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Valid reports whether the credentials are usable for signing.
func (c Credentials) Valid() bool {
	return c.AccessKeyID != "" && c.SecretAccessKey != ""
}

// Signer signs requests for one region and service.
type Signer struct {
	Region  string
	Service string
	// Now is injectable so signing is deterministic under test.
	Now func() time.Time
}

// NewSigner builds a signer for a region/service pair (for example
// "ap-south-1" and "s3").
func NewSigner(region, service string) *Signer {
	return &Signer{Region: region, Service: service, Now: time.Now}
}

func (s *Signer) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

// SignRequest adds the X-Amz-Date, X-Amz-Content-Sha256, optional
// X-Amz-Security-Token, and Authorization headers to req.
//
// payloadHash must be the hex SHA-256 of the request body (use
// EmptyPayloadHash for a bodyless request). The caller computes it because
// only the caller knows whether the body can be re-read.
func (s *Signer) SignRequest(req *http.Request, creds Credentials, payloadHash string) error {
	if !creds.Valid() {
		return fmt.Errorf("awssig: cannot sign without credentials")
	}
	if req.URL == nil {
		return fmt.Errorf("awssig: request has no URL")
	}

	now := s.now()
	amzDate := now.Format(amzDateFormat)
	shortDate := now.Format(shortDateFormat)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	req.Header.Set("Host", host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	signedHeaderNames, canonicalHeaders := canonicalizeHeaders(req.Header, host)

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL.Query()),
		canonicalHeaders,
		strings.Join(signedHeaderNames, ";"),
		payloadHash,
	}, "\n")

	scope := credentialScope(shortDate, s.Region, s.Service)
	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		scope,
		hashHex([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(
		signingKey(creds.SecretAccessKey, shortDate, s.Region, s.Service),
		[]byte(stringToSign),
	))

	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm,
		creds.AccessKeyID,
		scope,
		strings.Join(signedHeaderNames, ";"),
		signature,
	))
	return nil
}

// PresignGET returns a URL that authorizes a GET of rawURL for ttl without
// any credential in the request.
//
// This is how BridgeCore hands out usage exports: the S3 bucket stays
// private with public access blocked, and the client receives a capability
// that expires. Only the Host header is signed, so the URL works from a
// plain browser.
func (s *Signer) PresignGET(rawURL string, creds Credentials, ttl time.Duration) (string, error) {
	if !creds.Valid() {
		return "", fmt.Errorf("awssig: cannot presign without credentials")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("awssig: parse url: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("awssig: url has no host")
	}

	expires := int(ttl.Seconds())
	if expires < 1 {
		expires = 1
	}
	// SigV4 caps presigned URL lifetime at seven days.
	if expires > 604800 {
		expires = 604800
	}

	now := s.now()
	amzDate := now.Format(amzDateFormat)
	shortDate := now.Format(shortDateFormat)
	scope := credentialScope(shortDate, s.Region, s.Service)

	q := u.Query()
	q.Set("X-Amz-Algorithm", algorithm)
	q.Set("X-Amz-Credential", creds.AccessKeyID+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(expires))
	q.Set("X-Amz-SignedHeaders", "host")
	if creds.SessionToken != "" {
		q.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	canonicalRequest := strings.Join([]string{
		http.MethodGet,
		canonicalURI(u),
		canonicalQuery(q),
		"host:" + u.Host + "\n",
		"host",
		UnsignedPayload,
	}, "\n")

	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		scope,
		hashHex([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(
		signingKey(creds.SecretAccessKey, shortDate, s.Region, s.Service),
		[]byte(stringToSign),
	))

	q.Set("X-Amz-Signature", signature)
	u.RawQuery = canonicalQuery(q)
	return u.String(), nil
}

// HashPayload returns the hex SHA-256 of a request body.
func HashPayload(body []byte) string { return hashHex(body) }

func credentialScope(shortDate, region, service string) string {
	return strings.Join([]string{shortDate, region, service, terminator}, "/")
}

func signingKey(secret, shortDate, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(shortDate))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte(terminator))
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// canonicalizeHeaders returns the sorted signed-header names and the
// canonical header block. Host and every x-amz-* header must be signed;
// signing Content-Type as well binds the declared media type into the
// signature.
func canonicalizeHeaders(h http.Header, host string) ([]string, string) {
	values := map[string]string{"host": host}

	for name, vs := range h {
		lower := strings.ToLower(name)
		switch {
		case lower == "host":
			continue // already set from the URL
		case lower == "content-type", lower == "content-md5":
		case strings.HasPrefix(lower, "x-amz-"):
		default:
			continue
		}
		trimmed := make([]string, 0, len(vs))
		for _, v := range vs {
			trimmed = append(trimmed, strings.Join(strings.Fields(v), " "))
		}
		values[lower] = strings.Join(trimmed, ",")
	}

	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteString(":")
		b.WriteString(values[n])
		b.WriteString("\n")
	}
	return names, b.String()
}

// canonicalURI encodes the path one segment at a time, leaving separators
// intact. S3 expects single encoding here, unlike most other AWS services.
func canonicalURI(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		unescaped, err := url.PathUnescape(seg)
		if err != nil {
			unescaped = seg
		}
		segments[i] = uriEncode(unescaped, false)
	}
	joined := strings.Join(segments, "/")
	if !strings.HasPrefix(joined, "/") {
		joined = "/" + joined
	}
	return joined
}

// canonicalQuery renders query parameters sorted by key, then by value, with
// AWS's stricter percent-encoding rules.
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(q))
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			pairs = append(pairs, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(pairs, "&")
}

// uriEncode percent-encodes per RFC 3986 with AWS's rules: unreserved
// characters are never encoded, everything else always is (in uppercase
// hex), and "/" is preserved in paths but encoded in query values.
// net/url's encoders differ in exactly the ways that break a signature —
// most notably encoding a space as "+" — so this is done explicitly.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s) * 3)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return b.String()
}
