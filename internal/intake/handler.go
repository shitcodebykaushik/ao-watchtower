// Package intake provides a small authenticated HTTP boundary for GitHub webhooks.
package intake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shitcodebykaushik/ao-watchtower/internal/config"
	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
	"github.com/shitcodebykaushik/ao-watchtower/internal/github"
	"github.com/shitcodebykaushik/ao-watchtower/internal/ledger"
	"github.com/shitcodebykaushik/ao-watchtower/internal/policy"
)

const maxWebhookBody = 1 << 20

type Handler struct {
	secret    []byte
	config    config.Config
	ledger    *ledger.Ledger
	now       func() time.Time
	processor ReservationProcessor
}

// ReservationProcessor runs only after a new reservation has committed.
type ReservationProcessor interface {
	ProcessReservation(context.Context, ledger.Result, domain.CheckSuiteFacts) error
}

func NewHandler(secret []byte, configuration config.Config, durableLedger *ledger.Ledger, processors ...ReservationProcessor) (*Handler, error) {
	if len(secret) == 0 {
		return nil, fmt.Errorf("webhook secret is required")
	}
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	if durableLedger == nil {
		return nil, fmt.Errorf("ledger is required")
	}
	var processor ReservationProcessor
	if len(processors) > 1 {
		return nil, fmt.Errorf("only one reservation processor is supported")
	}
	if len(processors) == 1 {
		processor = processors[0]
	}
	return &Handler{secret: append([]byte(nil), secret...), config: configuration, ledger: durableLedger, now: time.Now, processor: processor}, nil
}

func (h *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if request.Header.Get("X-GitHub-Event") != "check_suite" {
		response.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(response, request.Body, maxWebhookBody))
	if err != nil {
		http.Error(response, "invalid webhook body", http.StatusBadRequest)
		return
	}
	// Signature verification deliberately precedes delivery validation and JSON decoding.
	if err := github.VerifySignature(h.secret, body, request.Header.Get("X-Hub-Signature-256")); err != nil {
		http.Error(response, "invalid webhook signature", http.StatusUnauthorized)
		return
	}
	deliveryID := request.Header.Get("X-GitHub-Delivery")
	if strings.TrimSpace(deliveryID) == "" || strings.TrimSpace(deliveryID) != deliveryID {
		http.Error(response, "invalid webhook delivery", http.StatusBadRequest)
		return
	}
	facts, err := github.NormalizeCheckSuiteCompleted(body)
	if err != nil {
		http.Error(response, "invalid check suite event", http.StatusBadRequest)
		return
	}
	evaluation, err := policy.EvaluateCheckSuite(facts, h.config)
	if err != nil {
		http.Error(response, "cannot evaluate check suite event", http.StatusBadRequest)
		return
	}
	digest := sha256.Sum256(body)
	result, err := h.ledger.RecordEvaluation(request.Context(), domain.WebhookDelivery{ID: deliveryID, PayloadDigest: hex.EncodeToString(digest[:]), ReceivedAt: h.now()}, facts, evaluation)
	if err != nil {
		http.Error(response, "cannot record check suite event", http.StatusInternalServerError)
		return
	}
	if result.Reserved && h.processor != nil {
		// A deferred investigation is not an intake failure: the reservation is
		// durable and will be replayed once capacity frees up. Reporting it as
		// an error would make every poll cycle look broken.
		if err := h.processor.ProcessReservation(request.Context(), result, facts); err != nil && !errors.Is(err, domain.ErrInvestigationDeferred) {
			http.Error(response, "webhook accepted but processing failed", http.StatusInternalServerError)
			return
		}
	}
	response.WriteHeader(http.StatusAccepted)
}
