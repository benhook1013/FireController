package reviewexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func ciWatchDiagnosticReason(t *testing.T, err error) string {
	t.Helper()
	var failure *core.Error
	if !errors.As(err, &failure) {
		t.Fatal("expected classified core failure", err)
	}
	switch details := failure.Details.(type) {
	case map[string]any:
		value, _ := details["reason"].(string)
		return value
	case map[string]string:
		return details["reason"]
	}
	return ""
}

func TestCIWatchRefusalClonesDetailsAndPreservesExistingClassification(t *testing.T) {
	for _, details := range []any{nil, map[string]string{"retained": "value"}, map[string]any{"retained": "value"}} {
		original := &core.Error{Code: "overloaded", Message: "Bounded local read refusal.", Status: 503, Details: details}
		before, _ := json.Marshal(original)
		got := ciWatchRefusal(fmt.Errorf("wrapped: %w", original), "ci_watch_binding_read")
		var cloned *core.Error
		if !errors.As(got, &cloned) || cloned == original || ciWatchDiagnosticReason(t, got) != "ci_watch_binding_read" || cloned.Code != original.Code || cloned.Status != original.Status || cloned.Message != original.Message {
			t.Fatal("refusal lost classification or shared its mutable error", got)
		}
		cloned.Details.(map[string]any)["retained"] = "changed"
		after, _ := json.Marshal(original)
		if string(before) != string(after) {
			t.Fatal("refusal annotation mutated original details")
		}
	}
	for _, original := range []*core.Error{
		{Code: "forbidden", Status: 403}, {Code: "validation_failed", Status: 400},
		{Code: "overloaded", Status: 503, Details: map[string]string{"reason": "result_wait_lifetime"}},
		{Code: "overloaded", Status: 503, Details: map[string]any{"reason": "already_classified"}},
	} {
		if ciWatchRefusal(original, "ci_watch_binding_read") != original {
			t.Fatal("existing authority, validation or reason was replaced", original)
		}
	}
}

func TestCIWatchHTTPRefusalIdentifiesLocalStageWithoutChangingOutcome(t *testing.T) {
	for _, phase := range []string{"scope_read", "observer", "capacity", "lifetime", "unauthorized", "validation"} {
		t.Run(phase, func(t *testing.T) {
			f, schedule, _, _, scope := ciWatchFixture(t)
			body := `{"targets":[{"pull_number":42,"head_sha":"` + strings.Repeat("a", 40) + `"}]}`
			if phase == "validation" {
				body = `{"targets":[]}`
			}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+attemptProject+"/reviews/ci/watch", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+f.reader)
			r.Header.Set("Prefer", "wait=0")
			want, code := "", 503
			switch phase {
			case "scope_read":
				release := holdCIWatchReadPool(t, f)
				defer release()
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
				defer cancel()
				r = r.WithContext(ctx)
				want = "ci_watch_scope_read"
			case "observer":
				f.e.lifecycleRunning.Store(false)
				want = "ci_watch_observer_unavailable"
			case "capacity":
				schedule.watchPrincipals = map[string]int{scope.PrincipalID: 8}
				want = "ci_watch_capacity"
			case "lifetime":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				f.e.resultWaitLifetime = ctx
				want = "result_wait_lifetime"
			case "unauthorized":
				r.Header.Del("Authorization")
				code = 401
			case "validation":
				code = 400
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			var wire struct {
				Error     core.Error `json:"error"`
				RequestID string     `json:"request_id"`
			}
			if w.Code != code || json.Unmarshal(w.Body.Bytes(), &wire) != nil || wire.RequestID == "" || ciWatchDiagnosticReason(t, &wire.Error) != want {
				t.Fatal("CI watch refusal lost safe stage or original status", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), f.reader) || strings.Contains(w.Body.String(), "SELECT ") {
				t.Fatal("refusal exposed secret or query")
			}
			if len(schedule.watchers) != 0 {
				t.Fatal("refused watch retained a subscription")
			}
		})
	}
}

func TestCIWatchBindingReadAndCallerCancellationKeepTheirOriginalBoundaries(t *testing.T) {
	f, schedule, _, reader, scope := ciWatchFixture(t)
	pins := []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}}
	before := lifecycleHintCounts(t, f)
	release := holdCIWatchReadPool(t, f)
	_, err := f.e.WatchCI(t.Context(), scope, pins, 0)
	release()
	if ciWatchDiagnosticReason(t, err) != "ci_watch_binding_read" {
		t.Fatal("binding read failure lost its stage", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.e.WatchCI(ctx, scope, pins, 300)
	if ciWatchDiagnosticReason(t, err) != "ci_watch_canceled" || len(schedule.watchers) != 0 || reader.calls != 0 || !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) {
		t.Fatal("cancellation changed watch acquisition, provider work or state", err)
	}
}
