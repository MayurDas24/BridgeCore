package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bridgecore/bridgecore/internal/exports"
	"github.com/bridgecore/bridgecore/pkg/awssig"
)

// sqsAPIVersion is the SQS query-protocol version. The query protocol is
// used rather than the newer JSON protocol because it is stable, trivially
// signable, and needs no generated client.
const sqsAPIVersion = "2012-11-05"

// SQSNotifier publishes export-job notifications to an Amazon SQS queue.
//
// SQS is what makes the pipeline event-driven rather than poll-driven in
// production: the API enqueues a job row and publishes a pointer to it, and
// a Lambda consumer wakes up immediately instead of a worker discovering the
// job on its next tick. The queue is deliberately carrying a pointer, not
// the work — see exports.JobNotification.
type SQSNotifier struct {
	queueURL string
	creds    *CredentialProvider
	signer   *awssig.Signer
	client   *http.Client
}

// NewSQSNotifier builds a notifier for a queue URL. The region is taken from
// the queue URL when it encodes one, falling back to the configured region.
func NewSQSNotifier(queueURL, fallbackRegion string, creds *CredentialProvider) (*SQSNotifier, error) {
	parsed, err := url.Parse(queueURL)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("cloud: invalid SQS queue URL %q", queueURL)
	}
	region := regionFromHost(parsed.Host, fallbackRegion)
	if region == "" {
		return nil, fmt.Errorf("cloud: could not determine the region for queue %q", queueURL)
	}

	return &SQSNotifier{
		queueURL: queueURL,
		creds:    creds,
		signer:   awssig.NewSigner(region, "sqs"),
		client:   &http.Client{Timeout: 10 * time.Second},
	}, nil
}

func (n *SQSNotifier) Backend() string { return "sqs" }

// Publish sends one notification.
//
// The caller treats a failure here as non-fatal: the job is already durably
// recorded in PostgreSQL, so a queue outage delays the export by one worker
// poll instead of losing it. That property is why the queue can be a plain
// best-effort optimisation rather than a second source of truth to keep
// consistent with the database.
func (n *SQSNotifier) Publish(ctx context.Context, notification exports.JobNotification) error {
	body, err := json.Marshal(notification)
	if err != nil {
		return err
	}

	form := url.Values{}
	form.Set("Action", "SendMessage")
	form.Set("Version", sqsAPIVersion)
	form.Set("MessageBody", string(body))
	encoded := form.Encode()

	creds, err := n.creds.Retrieve(ctx)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.queueURL, strings.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.ContentLength = int64(len(encoded))

	if err := n.signer.SignRequest(req, creds, awssig.HashPayload([]byte(encoded))); err != nil {
		return err
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("cloud: sqs send message: %w", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("cloud: sqs send message returned %s: %s", resp.Status, readErrorBody(resp))
	}
	return nil
}
