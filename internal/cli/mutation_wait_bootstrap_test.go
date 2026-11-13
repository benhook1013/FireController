package cli_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
)

func TestCLIExplicitMutationWaitReservesBootstrapAndHonorsCaller(t *testing.T) {
	for _, earlier := range []bool{false, true} {
		t.Run(map[bool]string{false: "receipt", true: "earlier-caller-deadline"}[earlier], func(t *testing.T) {
			f, project, worker := reviewExecutionFixture(t)
			var identities, writes atomic.Int64
			var committed atomic.Bool
			wantPrefer := "wait=10"
			if earlier {
				wantPrefer = "wait=0"
			}
			wantBody := `{"expected_configuration_revision":1,"pull_number":2,"channel":"cli","head_sha":"` + strings.Repeat("a", 40) + `"}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					if identities.Add(1) < 7 {
						http.Error(w, "synthetic gateway restart", 503)
						return
					}
					f.handler.ServeHTTP(w, r)
					return
				}
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/reviews/requests") {
					t.Error("unexpected request", r.Method, r.URL.Path)
					http.Error(w, "unexpected", 500)
					return
				}
				writes.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != wantBody || r.Header.Get("Idempotency-Key") != "explicit-review-key" || r.Header.Get("Prefer") != wantPrefer || r.Header.Get("X-FC-Worker") != worker || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("Authorization") != "Bearer "+f.admin {
					t.Error("mutation changed request/key/wait/binding", string(body), err)
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				// Commit through the actual public handler, then model a long receipt wait.
				// The selected Prefer value above is verified before skipping only the
				// fixture handler's own wait to control response timing exactly once.
				r.Header.Set("Prefer", "wait=0")
				receipt := httptest.NewRecorder()
				f.handler.ServeHTTP(receipt, r)
				if receipt.Code != http.StatusAccepted {
					t.Error("mutation did not commit", receipt.Code, receipt.Body.String())
				}
				committed.Store(true)
				select {
				case <-time.After(10 * time.Second):
				case <-r.Context().Done():
					return
				}
				for name, values := range receipt.Header() {
					for _, value := range values {
						w.Header().Add(name, value)
					}
				}
				w.WriteHeader(receipt.Code)
				w.Write(receipt.Body.Bytes())
			}))
			defer srv.Close()
			path := mutationProfiles(t, f, srv.URL, map[string][2]string{"builder": {project, worker}})
			args := reviewRequestArgs()
			args[len(args)-1] = "10"
			args = append([]string{"--profiles-file", path}, args...)
			ctx := t.Context()
			cancel := func() {}
			if earlier {
				ctx, cancel = context.WithTimeout(ctx, 9*time.Second)
			}
			defer cancel()
			var out, diagnostic bytes.Buffer
			start := time.Now()
			code := cli.RunContext(ctx, args, map[string]string{"FC_PROFILE": "builder"}, &out, &diagnostic)
			elapsed := time.Since(start)
			if identities.Load() != 7 || writes.Load() != 1 || !committed.Load() {
				t.Fatal("bootstrap or single-shot boundary changed", identities.Load(), writes.Load(), committed.Load(), code, diagnostic.String())
			}
			if earlier {
				if code == cli.ExitSuccess || out.Len() != 0 || !strings.Contains(diagnostic.String(), `"outcome_unknown":true`) || !strings.Contains(diagnostic.String(), "exact same idempotency key") || elapsed > 12*time.Second {
					t.Fatal("earlier caller deadline lost safe uncertainty", code, out.String(), diagnostic.String(), elapsed)
				}
			} else if code != cli.ExitSuccess || diagnostic.Len() != 0 || !strings.Contains(out.String(), `"id":`) || elapsed < 17*time.Second || elapsed > 24*time.Second {
				t.Fatal("slow bootstrap consumed selected mutation wait", code, out.String(), diagnostic.String(), elapsed)
			}
		})
	}
}
