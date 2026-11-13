package reviewexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/profile"
)

type ciDeadlineBody struct {
	io.ReadCloser
	once sync.Once
	read func()
}

func (b *ciDeadlineBody) Read(p []byte) (int, error) {
	b.once.Do(b.read)
	return b.ReadCloser.Read(p)
}

type ciDeadlineResponse struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *ciDeadlineResponse) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func ciDeadlineRequest(ctx context.Context, token string, seconds int) *http.Request {
	body := `{"targets":[{"pull_number":42,"head_sha":"` + strings.Repeat("a", 40) + `","base_sha":"` + strings.Repeat("b", 40) + `"}]}`
	r := httptest.NewRequestWithContext(ctx, "POST", "/api/v1/projects/"+attemptProject+"/reviews/ci/watch", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Prefer", "wait="+strconv.Itoa(seconds))
	r.Header.Set("X-FC-CI-Watch", "change-v3")
	return r
}

func ciDeadlinePending(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	var value CIWatchResult
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &value) != nil || !value.TimedOut || value.State != "pending" || len(value.Targets) != 1 {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
	item := value.Targets[0]
	if item.PullNumber != 42 || item.HeadSHA != strings.Repeat("a", 40) || item.BaseSHA != strings.Repeat("b", 40) || item.Reason != "ci_watch_check_pending" || item.ObservedAt != "" {
		t.Fatal("quiet response changed pins or invented observation", value)
	}
}

// Positive waits retain five seconds for final authorization and five for writing.
// Immediate reads retain the existing five-second response allowance.
func TestCIWatchHTTPDeadlineIncludesScopeAndBody(t *testing.T) {
	for _, test := range []struct {
		name                string
		seconds             int
		auth, body, caller  time.Duration
		elapsed, responseAt time.Duration
		refusal             string
	}{
		{name: "quiet", seconds: 120, elapsed: 120 * time.Second, responseAt: 130 * time.Second},
		{name: "slow_scope", seconds: 120, auth: 6 * time.Second, elapsed: 120 * time.Second, responseAt: 130 * time.Second},
		{name: "slow_body", seconds: 120, body: 6 * time.Second, elapsed: 120 * time.Second, responseAt: 130 * time.Second},
		{name: "scope_finishes_after_deadline", seconds: 3, auth: 4 * time.Second, elapsed: 4 * time.Second, responseAt: 13 * time.Second},
		{name: "body_finishes_after_deadline", seconds: 3, body: 4 * time.Second, elapsed: 4 * time.Second, responseAt: 13 * time.Second},
		{name: "zero_retains_immediate_read", auth: 4 * time.Second, elapsed: 4 * time.Second, responseAt: 9 * time.Second},
		{name: "zero_scope_exceeds_allowance", auth: 6 * time.Second, elapsed: 5 * time.Second, responseAt: 10 * time.Second, refusal: "ci_watch_scope_read"},
		{name: "earlier_caller_during_watch", seconds: 120, auth: 6 * time.Second, caller: 10 * time.Second, elapsed: 10 * time.Second, responseAt: 130 * time.Second, refusal: "ci_watch_canceled"},
		{name: "earlier_caller_during_scope", seconds: 120, auth: 6 * time.Second, caller: 2 * time.Second, elapsed: 2 * time.Second, responseAt: 130 * time.Second, refusal: "ci_watch_scope_read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, schedule, _, reader, _ := ciWatchFixture(t)
				before := lifecycleHintCounts(t, f)
				ctx := t.Context()
				if test.caller > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, test.caller)
					defer cancel()
				}
				r := ciDeadlineRequest(ctx, f.reader, test.seconds)
				var bodyAt time.Duration
				started := time.Now()
				r.Body = &ciDeadlineBody{ReadCloser: r.Body, read: func() { bodyAt = time.Since(started); time.Sleep(test.body) }}
				if test.auth > 0 {
					release := holdCIWatchReadPool(t, f)
					timer := time.AfterFunc(test.auth, release)
					defer timer.Stop()
					defer release()
				}
				response := &ciDeadlineResponse{ResponseRecorder: httptest.NewRecorder()}
				f.handler.ServeHTTP(response, r)
				if elapsed := time.Since(started); elapsed != test.elapsed {
					t.Fatalf("elapsed=%s want=%s body_read_at=%s", elapsed, test.elapsed, bodyAt)
				}
				if response.deadline.Sub(started) != test.responseAt {
					t.Fatalf("write deadline=%s want=%s", response.deadline.Sub(started), test.responseAt)
				}
				if test.refusal != "" {
					if response.Code != 503 || !strings.Contains(response.Body.String(), test.refusal) {
						t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
					}
				} else {
					ciDeadlinePending(t, response.ResponseRecorder)
					if bodyAt != test.auth || response.Header().Get("Preference-Applied") != "wait="+strconv.Itoa(test.seconds) {
						t.Fatal("authentication order or preference changed", bodyAt, response.Header())
					}
				}
				// A caller cancellation during Scope can precede the fixture's
				// scheduled release. Release before checking durable accounting.
				if test.refusal == "ci_watch_scope_read" {
					time.Sleep(test.auth - test.caller)
				}
				ciWatchRegistered(t, schedule, 0)
				reader.mu.Lock()
				calls := reader.calls
				reader.mu.Unlock()
				if calls != 0 || !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) {
					t.Fatal("watch called provider or mutated review accounting", calls)
				}
			})
		})
	}
}

func TestCIWatchHTTPDeadlinePreservesFinalFreshReadBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, schedule, _, reader, _ := ciWatchFixture(t)
		releaseAuth := holdCIWatchReadPool(t, f)
		started := time.Now()
		r := ciDeadlineRequest(t.Context(), f.reader, 120)
		response := &ciDeadlineResponse{ResponseRecorder: httptest.NewRecorder()}
		done := make(chan struct{})
		go func() { f.handler.ServeHTTP(response, r); close(done) }()
		synctest.Wait()
		time.Sleep(3500 * time.Millisecond)
		releaseAuth()
		synctest.Wait()
		ciWatchRegistered(t, schedule, 1)
		releaseFinal := holdCIWatchReadPool(t, f)
		time.Sleep(116500 * time.Millisecond)
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("quiet response skipped final fresh authorization")
		default:
		}
		time.Sleep(1750 * time.Millisecond)
		releaseFinal()
		<-done
		ciDeadlinePending(t, response.ResponseRecorder)
		if elapsed := time.Since(started); elapsed != 121750*time.Millisecond || response.deadline.Sub(started) != 130*time.Second {
			t.Fatalf("elapsed=%s deadline=%s", elapsed, response.deadline.Sub(started))
		}
		ciWatchRegistered(t, schedule, 0)
		reader.mu.Lock()
		calls := reader.calls
		reader.mu.Unlock()
		if calls != 0 {
			t.Fatal("provider calls", calls)
		}
	})
}

func TestCIWatchHTTPDeadlineLateBindingAdmissionDoesNotRestartWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, schedule, _, _, _ := ciWatchFixture(t)
		r := ciDeadlineRequest(t.Context(), f.reader, 3)
		started := time.Now()
		r.Body = &ciDeadlineBody{ReadCloser: r.Body, read: func() {
			release := holdCIWatchReadPool(t, f)
			time.AfterFunc(4*time.Second, release)
		}}
		response := httptest.NewRecorder()
		f.handler.ServeHTTP(response, r)
		ciDeadlinePending(t, response)
		if elapsed := time.Since(started); elapsed != 4*time.Second {
			t.Fatal("late binding restarted the wait", elapsed)
		}
		ciWatchRegistered(t, schedule, 0)
	})
}

func TestCIWatchDirectDeadlineStartsAtInvocation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, schedule, _, _, scope := ciWatchFixture(t)
		time.Sleep(6 * time.Second)
		started := time.Now()
		value, err := f.e.WatchCI(t.Context(), scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}}, 120)
		if err != nil || !value.TimedOut || value.State != "pending" || time.Since(started) != 120*time.Second {
			t.Fatal("direct wait contract changed", value, err, time.Since(started))
		}
		ciWatchRegistered(t, schedule, 0)
	})
}

// End-to-end strict-client control: a six-second auth gate used to add a full
// three-second server wait and miss the unchanged eight-second client budget.
func TestCIWatchHTTPDeadlineStrictClientKeepsPinsAndSingleRead(t *testing.T) {
	f, schedule, _, reader, _ := ciWatchFixture(t)
	schedule.readers["test-capability"] = &optionalCIWorkflowAttentionReader{ciWatchWorkflowReader: &ciWatchWorkflowReader{ciWatchReader: reader}, available: true}
	admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'deadline-reader','Reader')`, attemptProject)
	pins := []client.CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), AfterObservationKey: strings.Repeat("c", 64), FailureWorkflowIDs: []int64{123}}}
	wantBody, _ := json.Marshal(struct {
		Targets []client.CIWatchTarget `json:"targets"`
	}{pins})
	var requests atomic.Int64
	entered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			core.WriteJSON(w, 200, map[string]string{"instance_id": f.store.InstanceID()})
			return
		}
		if requests.Add(1) != 1 {
			t.Error("watch replayed")
			http.Error(w, "unexpected replay", 500)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/api/v1/projects/"+attemptProject+"/reviews/ci/watch" || r.Header.Get("Prefer") != "wait=3" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != "deadline-reader" || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("Authorization") != "Bearer "+f.reader {
			t.Error("watch changed binding or read contract")
		}
		var body bytes.Buffer
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.TeeReader(r.Body, &body), r.Body}
		close(entered)
		f.handler.ServeHTTP(w, r)
		if !bytes.Equal(body.Bytes(), wantBody) {
			t.Error("watch changed exact request body or pins")
		}
	}))
	defer srv.Close()
	credential := filepath.Join(t.TempDir(), "reader.token")
	if err := os.WriteFile(credential, []byte(f.reader), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(t.Context(), profile.Binding{Context: profile.Context{Profile: "fixture", ServerURL: srv.URL, InstanceID: f.store.InstanceID(), ProjectID: attemptProject, WorkerID: "deadline-reader"}, CredentialFile: credential})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	release := holdCIWatchReadPool(t, f)
	go func() { <-entered; time.Sleep(6 * time.Second); release() }()
	started := time.Now()
	value, err := c.WatchCI(t.Context(), pins, 3)
	elapsed := time.Since(started)
	if err != nil || value.State != "pending" || !value.TimedOut || len(value.Targets) != 1 || value.Targets[0].HeadSHA != pins[0].HeadSHA || value.Targets[0].BaseSHA != pins[0].BaseSHA {
		var failure *client.Error
		if errors.As(err, &failure) {
			t.Logf("read refusal code=%s status=%d details=%v", failure.Code, failure.Status, failure.Details)
		}
		t.Fatal("lost bounded quiet response", value, err, elapsed)
	}
	if elapsed < 6*time.Second || elapsed >= 8*time.Second || requests.Load() != 1 {
		t.Fatal("changed client budget or replayed request", elapsed, requests.Load())
	}
	ciWatchRegistered(t, schedule, 0)
	reader.mu.Lock()
	calls := reader.calls
	reader.mu.Unlock()
	if calls != 0 {
		t.Fatal("provider calls", calls)
	}
}
