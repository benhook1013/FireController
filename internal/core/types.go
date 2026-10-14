// Package core owns durable identity, access, mutation receipts and events.
package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
)

type Scope struct {
	PrincipalID string
	ProjectID   string
	WorkerID    string
	tokenHash   string
	instanceID  string
	admin       bool
}

type Mutation struct {
	Operation, Target, Key string
	Request                any
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`
	Details any    `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Message }
func Fail(code, message string, status int) error {
	return &Error{Code: code, Message: message, Status: status}
}

func NewID(prefix string) string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic("secure randomness unavailable")
	}
	return prefix + "_" + hex.EncodeToString(value[:])
}

func WriteJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		WriteError(w, Fail("internal_error", "The response could not be encoded.", 500))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if w.Header().Get("X-Request-ID") == "" {
		w.Header().Set("X-Request-ID", NewID("request"))
	}
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func WriteError(w http.ResponseWriter, err error) {
	var failure *Error
	if !errors.As(err, &failure) {
		failure = &Error{Code: "internal_error", Message: "The operation could not be completed.", Status: 500}
	}
	if w.Header().Get("X-Request-ID") == "" {
		w.Header().Set("X-Request-ID", NewID("request"))
	}
	WriteJSON(w, failure.Status, struct {
		Error     *Error `json:"error"`
		RequestID string `json:"request_id"`
	}{failure, w.Header().Get("X-Request-ID")})
}
