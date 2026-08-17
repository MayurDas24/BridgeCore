package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/bridgecore/bridgecore/pkg/awssig"
)

// SecretsManagerLoader resolves a bundle of secrets from AWS Secrets Manager.
//
// The secret is stored as a flat JSON object of environment variable names to
// values, so the application's configuration contract does not change between
// local development (a .env file) and production (a managed secret). The ECS
// task definition then contains no credentials at all — only the ARN of the
// secret and a task role permitted to read it, which means a leaked task
// definition, image, or CloudFormation template leaks nothing.
type SecretsManagerLoader struct {
	region string
	creds  *CredentialProvider
	signer *awssig.Signer
	client *http.Client
}

func NewSecretsManagerLoader(region string, creds *CredentialProvider) *SecretsManagerLoader {
	return &SecretsManagerLoader{
		region: region,
		creds:  creds,
		signer: awssig.NewSigner(region, "secretsmanager"),
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

type getSecretValueResponse struct {
	Name         string `json:"Name"`
	SecretString string `json:"SecretString"`
}

// LoadSecrets fetches and parses the secret identified by secretID (a name
// or a full ARN).
func (l *SecretsManagerLoader) LoadSecrets(secretID string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	payload, err := json.Marshal(map[string]string{"SecretId": secretID})
	if err != nil {
		return nil, err
	}

	creds, err := l.creds.Retrieve(ctx)
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("https://secretsmanager.%s.amazonaws.com/", l.region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "secretsmanager.GetSecretValue")
	req.ContentLength = int64(len(payload))

	if err := l.signer.SignRequest(req, creds, awssig.HashPayload(payload)); err != nil {
		return nil, err
	}

	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloud: get secret value: %w", err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("cloud: secret %q could not be read: %s", secretID, readErrorBody(resp))
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("cloud: get secret value returned %s", resp.Status)
	}

	var body getSecretValueResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("cloud: decode secret value: %w", err)
	}
	if body.SecretString == "" {
		return nil, fmt.Errorf("cloud: secret %q has no string value", secretID)
	}

	var values map[string]string
	if err := json.Unmarshal([]byte(body.SecretString), &values); err != nil {
		return nil, fmt.Errorf("cloud: secret %q must be a flat JSON object of KEY: value pairs: %w", secretID, err)
	}
	return values, nil
}
