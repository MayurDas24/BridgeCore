// Package cloud contains BridgeCore's AWS adapters: an S3-backed object
// store, an SQS notifier, and a Secrets Manager loader. Each implements an
// interface defined elsewhere in the codebase (exports.ObjectStore,
// exports.Notifier, config.SecretLoader), so nothing above this package
// depends on AWS at all — which is what lets the whole system run locally
// and in CI with no cloud account.
package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bridgecore/bridgecore/pkg/awssig"
)

// ecsCredentialsHost is the link-local address the ECS agent serves task
// role credentials from.
const ecsCredentialsHost = "http://169.254.170.2"

// credentialRefreshWindow is how long before expiry cached credentials are
// considered stale. Rotating early avoids a request being signed with
// credentials that expire in flight.
const credentialRefreshWindow = 5 * time.Minute

// CredentialProvider resolves AWS credentials, preferring an explicitly
// configured static pair (used only for MinIO/LocalStack in local testing)
// and otherwise fetching short-lived credentials from the ECS task role.
//
// Production deployments never carry a static AWS key: the ECS task assumes
// an IAM role whose policy is scoped to exactly the one bucket, one queue,
// and one secret this service needs, and the credentials it hands out expire
// on their own. That is the same reason GitHub Actions authenticates with
// OIDC rather than a stored access key — a credential that cannot be copied
// out and reused later is worth more than one that is merely secret.
type CredentialProvider struct {
	static awssig.Credentials
	client *http.Client

	mu       sync.RWMutex
	cached   awssig.Credentials
	expires  time.Time
	hasCache bool
}

// NewCredentialProvider builds a provider. Empty static credentials mean
// "resolve from the task role".
func NewCredentialProvider(accessKeyID, secretAccessKey, sessionToken string) *CredentialProvider {
	return &CredentialProvider{
		static: awssig.Credentials{
			AccessKeyID:     accessKeyID,
			SecretAccessKey: secretAccessKey,
			SessionToken:    sessionToken,
		},
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// Retrieve returns usable credentials, refreshing them when they are close
// to expiry.
func (p *CredentialProvider) Retrieve(ctx context.Context) (awssig.Credentials, error) {
	if p.static.Valid() {
		return p.static, nil
	}

	p.mu.RLock()
	if p.hasCache && time.Now().Before(p.expires.Add(-credentialRefreshWindow)) {
		creds := p.cached
		p.mu.RUnlock()
		return creds, nil
	}
	p.mu.RUnlock()

	creds, expires, err := p.fetchContainerCredentials(ctx)
	if err != nil {
		return awssig.Credentials{}, err
	}

	p.mu.Lock()
	p.cached = creds
	p.expires = expires
	p.hasCache = true
	p.mu.Unlock()

	return creds, nil
}

type containerCredentialsResponse struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
}

// fetchContainerCredentials reads credentials from the ECS credential
// endpoint. Both the relative form (Fargate) and the full-URI form are
// supported, since EKS and some local emulators use the latter.
func (p *CredentialProvider) fetchContainerCredentials(ctx context.Context) (awssig.Credentials, time.Time, error) {
	url := ""
	switch {
	case os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "":
		url = ecsCredentialsHost + os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")
	case os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "":
		url = os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI")
	default:
		return awssig.Credentials{}, time.Time{}, fmt.Errorf(
			"cloud: no AWS credentials available: set AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY for local testing, " +
				"or run on ECS with a task role")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return awssig.Credentials{}, time.Time{}, err
	}
	if token := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN"); token != "" {
		req.Header.Set("Authorization", token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return awssig.Credentials{}, time.Time{}, fmt.Errorf("cloud: fetch task role credentials: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return awssig.Credentials{}, time.Time{}, fmt.Errorf(
			"cloud: task role credential endpoint returned %s", resp.Status)
	}

	var body containerCredentialsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return awssig.Credentials{}, time.Time{}, fmt.Errorf("cloud: decode task role credentials: %w", err)
	}

	creds := awssig.Credentials{
		AccessKeyID:     body.AccessKeyID,
		SecretAccessKey: body.SecretAccessKey,
		SessionToken:    body.Token,
	}
	if !creds.Valid() {
		return awssig.Credentials{}, time.Time{}, fmt.Errorf("cloud: task role returned incomplete credentials")
	}

	expires := time.Now().Add(15 * time.Minute)
	if body.Expiration != "" {
		if parsed, err := time.Parse(time.RFC3339, body.Expiration); err == nil {
			expires = parsed
		}
	}
	return creds, expires, nil
}

// regionFromHost extracts the region from an AWS endpoint hostname, e.g.
// "sqs.ap-south-1.amazonaws.com" or "my-bucket.s3.ap-south-1.amazonaws.com".
// It returns fallback when the host does not carry one.
func regionFromHost(host, fallback string) string {
	host = strings.ToLower(host)
	if !strings.HasSuffix(host, ".amazonaws.com") {
		return fallback
	}
	parts := strings.Split(strings.TrimSuffix(host, ".amazonaws.com"), ".")
	for i := len(parts) - 1; i >= 0; i-- {
		if looksLikeRegion(parts[i]) {
			return parts[i]
		}
	}
	return fallback
}

// looksLikeRegion recognises the "<geo>-<direction>-<n>" shape of an AWS
// region without needing a hardcoded list that goes stale every time AWS
// opens a new one.
func looksLikeRegion(s string) bool {
	segments := strings.Split(s, "-")
	if len(segments) < 3 {
		return false
	}
	last := segments[len(segments)-1]
	if len(last) != 1 || last[0] < '0' || last[0] > '9' {
		return false
	}
	for _, seg := range segments[:len(segments)-1] {
		if seg == "" {
			return false
		}
		for i := 0; i < len(seg); i++ {
			if seg[i] < 'a' || seg[i] > 'z' {
				return false
			}
		}
	}
	return true
}
