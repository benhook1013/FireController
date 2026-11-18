package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
	"net/http"
	"sort"

	"github.com/benhook1013/FireController/internal/opvarsmodel"
)

// OperatorVariable is one effective value: a project override, else the
// instance value. FireController never interprets it.
type OperatorVariable struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Scope     string `json:"scope"`
	Revision  int64  `json:"revision"`
	UpdatedAt string `json:"updated_at"`
}

type OperatorVariables struct {
	Variables []OperatorVariable `json:"variables"`
}

// OperatorVariableEntry is one scope's current state; Set is false after an unset.
type OperatorVariableEntry struct {
	Set       bool   `json:"set"`
	Value     string `json:"value,omitempty"`
	Revision  int64  `json:"revision"`
	ActorID   string `json:"actor_id"`
	UpdatedAt string `json:"updated_at"`
}

// OperatorVariableDetail omits a scope that has never written the key.
type OperatorVariableDetail struct {
	Key      string                 `json:"key"`
	Set      bool                   `json:"set"`
	Value    string                 `json:"value,omitempty"`
	Scope    string                 `json:"scope,omitempty"`
	Instance *OperatorVariableEntry `json:"instance,omitempty"`
	Project  *OperatorVariableEntry `json:"project,omitempty"`
}

type OperatorVariableReceipt struct {
	Key      string `json:"key"`
	Scope    string `json:"scope"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}

type OperatorVariableRevision struct {
	Revision  int64  `json:"revision"`
	Set       bool   `json:"set"`
	Value     string `json:"value,omitempty"`
	ActorID   string `json:"actor_id"`
	CreatedAt string `json:"created_at"`
}

// SetOperatorVariableRequest writes one scope. A nil Value unsets it; a nil
// ExpectedRevision is last-write-wins with full history.
type SetOperatorVariableRequest struct {
	Scope            string  `json:"scope"`
	Value            *string `json:"value"`
	ExpectedRevision *int64  `json:"expected_revision,omitempty"`
}

// ValidOperatorVariableKey exposes the shared key bound for local validation.
func ValidOperatorVariableKey(key string) bool { return opvarsmodel.ValidKey(key) }

// ValidOperatorVariableValue exposes the shared value bound for local validation.
func ValidOperatorVariableValue(value string) bool { return opvarsmodel.ValidValue(value) }

// NewOperatorVariableKey returns a fresh idempotency key for one light write.
// Variable writes are state replacements, so a retry with a new key converges.
func NewOperatorVariableKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "vars-" + hex.EncodeToString(b[:]), nil
}

func validVariableEntry(e *OperatorVariableEntry) bool {
	return e == nil || e.Revision >= 1 && validID(e.ActorID) && validJobProgressTime(e.UpdatedAt) && (e.Set && opvarsmodel.ValidValue(e.Value) || !e.Set && e.Value == "")
}

// OperatorVariables reads the complete effective set for the bound project.
func (c *Client) OperatorVariables(ctx context.Context) (OperatorVariables, error) {
	var result OperatorVariables
	if err := c.get(ctx, c.projectPath("vars"), nil, true, &result); err != nil {
		return OperatorVariables{}, err
	}
	if result.Variables == nil || len(result.Variables) > 2*opvarsmodel.MaxKeys || !sort.SliceIsSorted(result.Variables, func(i, j int) bool { return result.Variables[i].Key < result.Variables[j].Key }) {
		return OperatorVariables{}, invalidResponse()
	}
	for i, v := range result.Variables {
		if !opvarsmodel.ValidKey(v.Key) || !opvarsmodel.ValidValue(v.Value) || !opvarsmodel.ValidScope(v.Scope) || v.Revision < 1 || !validJobProgressTime(v.UpdatedAt) || i > 0 && result.Variables[i-1].Key == v.Key {
			return OperatorVariables{}, invalidResponse()
		}
	}
	return result, nil
}

// Values returns the effective set as a compact key/value map.
func (v OperatorVariables) Values() map[string]string {
	out := make(map[string]string, len(v.Variables))
	for _, item := range v.Variables {
		out[item.Key] = item.Value
	}
	return out
}

// OperatorVariable reads one key's effective value and both scopes behind it.
func (c *Client) OperatorVariable(ctx context.Context, key string) (OperatorVariableDetail, error) {
	if !opvarsmodel.ValidKey(key) {
		return OperatorVariableDetail{}, validation()
	}
	var result OperatorVariableDetail
	if err := c.get(ctx, c.projectPath("vars", key), nil, true, &result); err != nil {
		return OperatorVariableDetail{}, err
	}
	effective := result.Instance
	if result.Project != nil && result.Project.Set {
		effective = result.Project
	}
	if result.Key != key || !validVariableEntry(result.Instance) || !validVariableEntry(result.Project) || result.Set != (effective != nil && effective.Set) || result.Set && (!opvarsmodel.ValidScope(result.Scope) || result.Value != effective.Value) || !result.Set && (result.Scope != "" || result.Value != "") {
		return OperatorVariableDetail{}, invalidResponse()
	}
	return result, nil
}

// SetOperatorVariable writes or unsets one scope's value for key.
func (c *Client) SetOperatorVariable(ctx context.Context, idempotencyKey, key string, request SetOperatorVariableRequest) (OperatorVariableReceipt, error) {
	if !validMutationKey(idempotencyKey) || !opvarsmodel.ValidKey(key) || !opvarsmodel.ValidScope(request.Scope) || request.Value != nil && !opvarsmodel.ValidValue(*request.Value) || request.ExpectedRevision != nil && (*request.ExpectedRevision < 0 || *request.ExpectedRevision == math.MaxInt64) {
		return OperatorVariableReceipt{}, mutationValidation()
	}
	var result OperatorVariableReceipt
	if err := c.mutate(ctx, http.MethodPut, c.projectPath("vars", key), idempotencyKey, request, http.StatusOK, &result); err != nil {
		return OperatorVariableReceipt{}, err
	}
	if result.Key != key || result.Scope != request.Scope || result.Revision < 0 || result.Outcome != "updated" && result.Outcome != "unchanged" || result.Outcome == "updated" && result.Revision < 1 || request.ExpectedRevision != nil && (result.Outcome == "updated" && result.Revision != *request.ExpectedRevision+1 || result.Outcome == "unchanged" && result.Revision != *request.ExpectedRevision) {
		return OperatorVariableReceipt{}, unknownMutation("invalid_response", "The service returned an invalid operator variable receipt; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}

// OperatorVariableHistory reads one newest-first page of a key in one scope.
func (c *Client) OperatorVariableHistory(ctx context.Context, key, scope string, options PageOptions) (Page[OperatorVariableRevision], error) {
	if !opvarsmodel.ValidKey(key) || !opvarsmodel.ValidScope(scope) {
		return Page[OperatorVariableRevision]{}, validation()
	}
	options, q, err := projectMapPage(options)
	if err != nil {
		return Page[OperatorVariableRevision]{}, err
	}
	q.Set("scope", scope)
	var result Page[OperatorVariableRevision]
	if err = c.get(ctx, c.projectPath("vars", key, "history"), q, true, &result); err != nil {
		return Page[OperatorVariableRevision]{}, err
	}
	if !validPage(result, options) || result.Cursor != "" && !validOpaqueCursor(result.Cursor) {
		return Page[OperatorVariableRevision]{}, invalidResponse()
	}
	var previous int64
	for _, item := range result.Items {
		e := OperatorVariableEntry{item.Set, item.Value, item.Revision, item.ActorID, item.CreatedAt}
		if !validVariableEntry(&e) || previous != 0 && item.Revision >= previous {
			return Page[OperatorVariableRevision]{}, invalidResponse()
		}
		previous = item.Revision
	}
	return result, nil
}

// DoctorOperatorVariables returns the effective values for session-start
// diagnostics, or nil when the service cannot provide them (for example an
// older daemon). Doctor stays usable; the field is simply absent.
func (c *Client) DoctorOperatorVariables(ctx context.Context) *map[string]string {
	vars, err := c.OperatorVariables(ctx)
	if err != nil {
		return nil
	}
	values := vars.Values()
	return &values
}
