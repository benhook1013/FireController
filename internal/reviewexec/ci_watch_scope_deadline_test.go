package reviewexec

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCIWatchHTTPScopeReadContentionIsBounded(t *testing.T) {
	for _, mode := range []string{"zero-wait", "caller-deadline"} {
		t.Run(mode, func(t *testing.T) {
			f, s, _, reader, _ := ciWatchFixture(t)
			release := holdCIWatchReadPool(t, f)
			defer release()
			ctx := t.Context()
			if mode == "caller-deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			req := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/ci/watch", strings.NewReader(`{"targets":[{"pull_number":42,"head_sha":"`+strings.Repeat("a", 40)+`"}]}`)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+f.reader)
			req.Header.Set("Prefer", "wait=0")
			w := httptest.NewRecorder()
			done := make(chan struct{})
			started := time.Now()
			go func() { defer close(done); f.handler.ServeHTTP(w, req) }()
			select {
			case <-done:
			case <-time.After(7 * time.Second):
				release()
				<-done
				t.Fatal("scope read exceeded bounded HTTP allowance")
			}
			elapsed := time.Since(started)
			if mode == "zero-wait" && (elapsed < 4500*time.Millisecond || elapsed > 6500*time.Millisecond) {
				t.Fatal("wrong scope deadline", elapsed)
			}
			if mode == "caller-deadline" && elapsed > time.Second {
				t.Fatal("ignored caller deadline", elapsed)
			}
			if w.Code != 503 || !strings.Contains(w.Body.String(), "ci_watch_scope_read") {
				t.Fatal("unscoped storage refusal", w.Code, w.Body.String())
			}
			ciWatchRegistered(t, s, 0)
			if reader.calls != 0 {
				t.Fatal("scope refusal acquired provider work")
			}
		})
	}
}
