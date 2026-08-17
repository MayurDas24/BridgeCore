package cloud

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bridgecore/bridgecore/internal/exports"
	"github.com/bridgecore/bridgecore/pkg/awssig"
)

// S3Store is an exports.ObjectStore backed by Amazon S3.
//
// The bucket is private with public access blocked (see
// infra/terraform/s3.tf): the only way a client ever reads an object is
// through a presigned GET minted per download request, which expires. No
// object is ever made public, and no long-lived URL is ever stored.
type S3Store struct {
	bucket   string
	region   string
	endpoint string // set for MinIO/LocalStack; empty means real AWS
	creds    *CredentialProvider
	signer   *awssig.Signer
	client   *http.Client
}

// NewS3Store builds an S3-backed store. endpoint is normally empty; setting
// it switches to path-style addressing for S3-compatible emulators.
func NewS3Store(bucket, region, endpoint string, creds *CredentialProvider) *S3Store {
	return &S3Store{
		bucket:   bucket,
		region:   region,
		endpoint: strings.TrimSuffix(endpoint, "/"),
		creds:    creds,
		signer:   awssig.NewSigner(region, "s3"),
		client: &http.Client{
			// Generous, because an export upload can be tens of megabytes,
			// but bounded, because a hung upload must not pin a worker.
			Timeout: 2 * time.Minute,
		},
	}
}

func (s *S3Store) Backend() string { return "s3" }

// objectURL builds the addressable URL for a key. Virtual-hosted style is
// used against real S3 (it is the only style AWS still recommends);
// path-style is used against an emulator, which typically cannot do
// wildcard DNS.
func (s *S3Store) objectURL(key string) string {
	escaped := escapeKeyPath(key)
	if s.endpoint != "" {
		return fmt.Sprintf("%s/%s/%s", s.endpoint, s.bucket, escaped)
	}
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", s.bucket, s.region, escaped)
}

// escapeKeyPath percent-encodes each key segment while keeping the "/"
// separators, matching how S3 addresses nested keys. net/url's escapers are
// not used here because they encode a space as "+", which S3 treats as a
// literal plus and which would break the signature.
func escapeKeyPath(key string) string {
	segments := strings.Split(strings.TrimPrefix(key, "/"), "/")
	for i, seg := range segments {
		segments[i] = escapeKeySegment(seg)
	}
	return strings.Join(segments, "/")
}

func escapeKeySegment(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return b.String()
}

// Put uploads an object.
//
// SigV4 requires the SHA-256 of the payload before the request is sent, so
// the body must be readable twice. Callers pass an *os.File (the worker's
// temp file), which seeks cheaply; anything else is buffered as a fallback,
// which is the one path where a large export would cost memory.
func (s *S3Store) Put(ctx context.Context, key, contentType string, body io.Reader) (int64, error) {
	payload, size, err := materialize(body)
	if err != nil {
		return 0, err
	}

	creds, err := s.creds.Retrieve(ctx)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.objectURL(key), payload.reader())
	if err != nil {
		return 0, err
	}
	req.ContentLength = size
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// Server-side encryption is also enforced by the bucket policy; setting
	// it here means a request that somehow bypassed the policy still stores
	// encrypted.
	req.Header.Set("X-Amz-Server-Side-Encryption", "AES256")

	if err := s.signer.SignRequest(req, creds, payload.sha256Hex()); err != nil {
		return 0, err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cloud: s3 put %q: %w", key, err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("cloud: s3 put %q returned %s: %s", key, resp.Status, readErrorBody(resp))
	}
	return size, nil
}

// Open downloads an object. Used only by administrative tooling: normal
// downloads go straight from the client to S3 via a presigned URL, so the
// bytes never pass through the API and never occupy an ECS task.
func (s *S3Store) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	creds, err := s.creds.Retrieve(ctx)
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.objectURL(key), nil)
	if err != nil {
		return nil, 0, err
	}
	if err := s.signer.SignRequest(req, creds, awssig.EmptyPayloadHash); err != nil {
		return nil, 0, err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("cloud: s3 get %q: %w", key, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		drainAndClose(resp)
		return nil, 0, exports.ErrObjectNotFound
	}
	if resp.StatusCode/100 != 2 {
		defer drainAndClose(resp)
		return nil, 0, fmt.Errorf("cloud: s3 get %q returned %s", key, resp.Status)
	}
	return resp.Body, resp.ContentLength, nil
}

// PresignGet mints a time-limited download URL for a private object.
func (s *S3Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	creds, err := s.creds.Retrieve(ctx)
	if err != nil {
		return "", err
	}
	return s.signer.PresignGET(s.objectURL(key), creds, ttl)
}

// payloadBuffer holds a request body that can be replayed for signing.
type payloadBuffer struct {
	data []byte
}

func (p *payloadBuffer) reader() io.Reader { return bytes.NewReader(p.data) }
func (p *payloadBuffer) sha256Hex() string { return awssig.HashPayload(p.data) }

func materialize(body io.Reader) (*payloadBuffer, int64, error) {
	if body == nil {
		return &payloadBuffer{}, 0, nil
	}
	if seeker, ok := body.(io.Seeker); ok {
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return nil, 0, err
		}
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, 0, err
	}
	return &payloadBuffer{data: data}, int64(len(data)), nil
}

func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

// readErrorBody returns a bounded slice of an AWS error response, which is
// XML or JSON describing why the call failed. It is bounded because an error
// body is not a place to spend unbounded memory or unbounded log volume.
func readErrorBody(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return strings.TrimSpace(string(data))
}
