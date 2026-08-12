// Package github verifies and normalizes the narrow GitHub webhook surface Watchtower accepts.
package github

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/agent-orchestrator/ao-watchtower/internal/domain"
)

const signaturePrefix = "sha256="

// VerifySignature authenticates the unmodified request body before it is parsed.
func VerifySignature(secret, body []byte, signature string) error {
	if len(secret) == 0 {
		return fmt.Errorf("webhook secret is required")
	}
	if !strings.HasPrefix(signature, signaturePrefix) {
		return fmt.Errorf("missing or unsupported webhook signature")
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(signature, signaturePrefix))
	if err != nil || len(provided) != sha256.Size {
		return fmt.Errorf("invalid webhook signature")
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return fmt.Errorf("invalid webhook signature")
	}
	return nil
}

type checkSuiteEvent struct {
	Action     string `json:"action"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	CheckSuite struct {
		ID            int64  `json:"id"`
		Conclusion    string `json:"conclusion"`
		HeadSHA       string `json:"head_sha"`
		DetailsURL    string `json:"details_url"`
		CheckSuiteURL string `json:"url"`
		PullRequests  []struct {
			Number int64 `json:"number"`
			Head   struct {
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_requests"`
	} `json:"check_suite"`
}

// NormalizeCheckSuiteCompleted accepts exactly one PR association and only the facts used by the MVP rule.
func NormalizeCheckSuiteCompleted(payload []byte) (domain.CheckSuiteFacts, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var event checkSuiteEvent
	if err := decoder.Decode(&event); err != nil {
		return domain.CheckSuiteFacts{}, fmt.Errorf("decode check_suite event: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return domain.CheckSuiteFacts{}, fmt.Errorf("decode check_suite event: multiple JSON values")
	}
	if event.Action != "completed" {
		return domain.CheckSuiteFacts{}, fmt.Errorf("event action must be completed")
	}
	repository, err := domain.ParseRepository(event.Repository.Owner.Login + "/" + event.Repository.Name)
	if err != nil {
		return domain.CheckSuiteFacts{}, fmt.Errorf("repository: %w", err)
	}
	if event.CheckSuite.ID <= 0 {
		return domain.CheckSuiteFacts{}, fmt.Errorf("check suite id must be positive")
	}
	if len(event.CheckSuite.PullRequests) != 1 {
		return domain.CheckSuiteFacts{}, fmt.Errorf("check suite must refer to exactly one pull request")
	}
	pullRequest := event.CheckSuite.PullRequests[0]
	if pullRequest.Number <= 0 {
		return domain.CheckSuiteFacts{}, fmt.Errorf("pull request number must be positive")
	}
	if !sameSHA(event.CheckSuite.HeadSHA, pullRequest.Head.SHA) {
		return domain.CheckSuiteFacts{}, fmt.Errorf("check suite and pull request head SHAs must match")
	}
	facts := domain.CheckSuiteFacts{ProviderID: event.CheckSuite.ID, Repository: repository, PullNumber: pullRequest.Number, HeadSHA: strings.ToLower(event.CheckSuite.HeadSHA), Conclusion: event.CheckSuite.Conclusion, DetailsURL: event.CheckSuite.DetailsURL, CheckSuiteURL: event.CheckSuite.CheckSuiteURL}
	if err := facts.Validate(); err != nil {
		return domain.CheckSuiteFacts{}, err
	}
	if !validProviderURL(facts.DetailsURL) || !validProviderURL(facts.CheckSuiteURL) {
		return domain.CheckSuiteFacts{}, fmt.Errorf("check suite URL must be an absolute HTTP(S) URL without credentials or fragments")
	}
	return facts, nil
}

func sameSHA(first, second string) bool {
	return strings.EqualFold(strings.TrimSpace(first), strings.TrimSpace(second))
}

func validProviderURL(value string) bool {
	if value == "" {
		return true
	}
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}
