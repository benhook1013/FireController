package core

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"
)

// Private timing metadata leaves the original error and public response intact.
// It contains no mutation key, operation/target identity, input or raw error.
type mutationOverload struct {
	err                error
	stage, cause       string
	elapsed, admission int64
}

func (e *mutationOverload) Error() string { return e.err.Error() }
func (e *mutationOverload) Unwrap() error { return e.err }

func observeMutationOverload(err, cause error, stage string, elapsed, admission time.Duration) error {
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "overloaded" || failure.Status != 503 {
		return err
	}
	class := "unknown"
	switch {
	case errors.Is(cause, context.DeadlineExceeded):
		class = "deadline"
	case errors.Is(cause, context.Canceled):
		class = "canceled"
	default:
		var coded interface{ Code() int }
		if errors.As(cause, &coded) && (coded.Code()&255 == 5 || coded.Code()&255 == 6) {
			class = "sqlite_busy"
		}
	}
	return &mutationOverload{err, stage, class, elapsed.Milliseconds(), admission.Milliseconds()}
}

func logMutationOverload(err error, requestID string) {
	var observation *mutationOverload
	if !errors.As(err, &observation) {
		return
	}
	// The response normally generated this identifier. Do not reflect arbitrary
	// preexisting response-header text into a structured operational log.
	hexID := strings.TrimPrefix(requestID, "request_")
	if len(hexID) != 32 || requestID != "request_"+hexID || strings.ToLower(hexID) != hexID {
		return
	}
	if _, err := hex.DecodeString(hexID); err != nil {
		return
	}
	entry := struct {
		Event       string `json:"event"`
		RequestID   string `json:"request_id"`
		Stage       string `json:"stage"`
		Cause       string `json:"cause"`
		ElapsedMS   int64  `json:"elapsed_ms"`
		AdmissionMS int64  `json:"admission_ms"`
	}{"mutation_overloaded", requestID, observation.stage, observation.cause, observation.elapsed, observation.admission}
	body, _ := json.Marshal(entry)
	log.Print(string(body))
}
