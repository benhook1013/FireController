//go:build packaged_smoke

package reviewexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

// This gate consumes an already built daemon. Fixture construction establishes
// controlled, stopped actions; the service only records the operator assertion.
func TestPackagedEndedSettlementRetainsOriginalAuthority(t *testing.T) {
	bin := os.Getenv("FC_SMOKE_BIN_DIR")
	if !filepath.IsAbs(bin) {
		t.Fatal("FC_SMOKE_BIN_DIR must select absolute, already built inputs")
	}
	binary := filepath.Join(bin, "fcd")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	input, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal("packaged daemon unavailable")
	}
	digest := sha256.Sum256(input)
	t.Logf("ended settlement packaged input fcd sha256=%x", digest)
	t.Cleanup(func() {
		after, err := os.ReadFile(binary)
		if err != nil || sha256.Sum256(after) != digest {
			t.Error("packaged settlement modified its input daemon")
		}
	})
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			t.Cleanup(cancel)
			f, a := endedSettlementFixture(t, channel)
			f.configure(t, otherAttemptProject, "other-account", 0, false)
			other := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "unrelated-reservation", "cli", 4, 1))
			other, err := f.e.Admit(ctx, otherAttemptProject, other.ID, other.Revision, attemptAnchor(otherAttemptProject, 4), "")
			if err != nil {
				t.Fatal("unrelated controlled admission failed")
			}
			other, err = f.e.Advance(ctx, otherAttemptProject, other.ID, other.Revision, "uncertain", "dispatch_interrupted", "")
			if err != nil {
				t.Fatal("unrelated controlled uncertainty failed")
			}
			otherBefore := attemptState(t, f, other)
			before := transferAuthorityRows(t, f, true)
			reservations := attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`)
			if reservations != 2 {
				t.Fatal("fixture must retain both exact reservations")
			}
			var database string
			if err := f.store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&database); err != nil || !filepath.IsAbs(database) {
				t.Fatal("owned fixture database location unavailable")
			}
			data := filepath.Dir(database)
			instance := f.store.InstanceID()
			if err := f.store.Close(); err != nil {
				t.Fatal("fixture did not stop before packaged startup")
			}
			var external atomic.Int64
			deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { external.Add(1); w.WriteHeader(http.StatusBadGateway) }))
			t.Cleanup(deny.Close)
			env := []string{"HOME=" + data, "USERPROFILE=" + data, "TMPDIR=" + data, "TEMP=" + data, "TMP=" + data, "PATH=", "HTTP_PROXY=" + deny.URL, "HTTPS_PROXY=" + deny.URL, "NO_PROXY=127.0.0.1,localhost"}
			for _, key := range []string{"SYSTEMROOT", "WINDIR"} {
				if value := os.Getenv(key); value != "" {
					env = append(env, key+"="+value)
				}
			}
			body := []byte(encoded(hostedSettlementBody(a)))
			key := "packaged-ended-settlement"
			first := startEndedSettlementDaemon(t, ctx, binary, data, instance, env)
			status, receipt := first.request(t, ctx, instance, f.admin, key, hostedSettlementPath(attemptProject, a), body)
			if status != http.StatusOK {
				t.Fatalf("packaged %s settlement refused: HTTP %d", channel, status)
			}
			first.stop(t)
			reopenEndedSettlement(t, ctx, f, data)
			wantState, wantReason := "failed", "cli_partial_settled"
			if channel == "hosted" {
				wantState, wantReason = "cancelled", "operator_verified_external_stop"
			}
			var settled attemptSettlementReceipt
			if json.Unmarshal(receipt, &settled) != nil || settled.AttemptID != a.ID || settled.OperationID != a.OperationID || settled.Revision != a.Revision+1 || settled.State != wantState || settled.Outcome != "settled" {
				t.Fatal("packaged settlement receipt lost exact identity or disposition")
			}
			after := transferAuthorityRows(t, f, true)
			for _, table := range []string{"review_execution_evidence", "review_hosted_observations", "review_adjudication_state", "review_adjudication_changes", "review_finding_routes", "review_finding_route_revisions", "review_route_policy_facts", "review_route_policy_invalidations", "review_run_versions", "review_finding_versions", "review_execution_config", "review_round_controls", "review_round_changes", "review_round_completions"} {
				if original, existed := before[table]; existed && !reflect.DeepEqual(original, after[table]) || !existed && len(after[table]) != 0 {
					t.Fatal("packaged settlement changed retained source, findings, routes or controls", table)
				}
			}
			if attemptState(t, f, other) != otherBefore || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != reservations-1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 {
				t.Fatal("packaged settlement released anything except its exact reservation")
			}
			var intent, state, reason string
			if err := f.store.DB().QueryRow(`SELECT a.intent,o.state,o.reason FROM review_execution_attempts a JOIN review_operations o ON o.id=a.operation_id WHERE a.id=?`, a.ID).Scan(&intent, &state, &reason); err != nil || intent != a.Intent || state != wantState || reason != wantReason || attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE state='completed'`) != 0 {
				t.Fatal("packaged settlement changed intent or supplied completion")
			}
			plan := SourcePlan{Repository: reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, Pull: reviewprovider.Pull{RepositoryID: 801, Number: 2, State: "open", HeadSHA: a.Anchor.CandidateSHA, BaseSHA: a.Anchor.ParentSHA, BaseRef: "main", ChangedFiles: 1}, ParentSHA: a.Anchor.ParentSHA, ObservedAt: stamp()}
			if covered, err := f.e.Coverage(ctx, attemptProject, plan, channel); err != nil || covered {
				t.Fatal("packaged settlement granted completed coverage")
			}
			if channel == "cli" {
				var evidence string
				if err := f.store.DB().QueryRow(`SELECT id FROM review_execution_evidence WHERE attempt_id=?`, a.ID).Scan(&evidence); err != nil {
					t.Fatal("original CLI evidence missing")
				}
				detail := cliPublicationDetail(t, f, 2, evidence)
				if len(detail.Findings) != 4 || detail.Run.Complete || detail.Run.Attributable || detail.Run.Finalized {
					t.Fatal("packaged settlement lost partial findings or supplied credit")
				}
			}
			// Compare only retained review authority and mutation history across
			// the restart; unrelated daemon bookkeeping is not settlement work.
			replayBefore := transferAuthorityRows(t, f, false)
			for _, table := range []string{"events", "receipts", "review_run_versions", "review_finding_versions", "review_pull_versions", "review_attempt_versions", "review_snapshots", "review_round_controls", "review_round_changes", "review_round_completions", "review_finding_routes", "review_finding_route_revisions"} {
				replayBefore[table] = after[table]
			}
			if err := f.store.Close(); err != nil {
				t.Fatal("fixture did not stop before replay startup")
			}
			second := startEndedSettlementDaemon(t, ctx, binary, data, instance, env)
			status, replay := second.request(t, ctx, instance, f.admin, key, hostedSettlementPath(attemptProject, a), body)
			if status != http.StatusOK || !bytes.Equal(receipt, replay) {
				t.Fatal("packaged exact-key replay after restart changed its receipt")
			}
			changed := hostedSettlementBody(a)
			changed["evidence_reference"] = "operator-record:changed-reference"
			status, conflict := second.request(t, ctx, instance, f.admin, key, hostedSettlementPath(attemptProject, a), []byte(encoded(changed)))
			if status != http.StatusConflict || !bytes.Contains(conflict, []byte(`"idempotency_conflict"`)) {
				t.Fatal("packaged changed-body retry did not conflict")
			}
			second.stop(t)
			reopenEndedSettlement(t, ctx, f, data)
			replayAfter := transferAuthorityRows(t, f, true)
			for table, original := range replayBefore {
				if !reflect.DeepEqual(original, replayAfter[table]) {
					t.Fatal("packaged replay changed retained review authority or audit history", table)
				}
			}
			if external.Load() != 0 || attemptState(t, f, other) != otherBefore {
				t.Fatal("packaged settlement contacted a provider or changed unrelated work")
			}
			t.Log("packaged ended settlement passed: daemon_starts=2 settlement_posts=3 external_http=0 owned_processes=0 immutable_source=true completed_credit=false")
		})
	}
}

func reopenEndedSettlement(t *testing.T, ctx context.Context, f *attemptFixture, data string) {
	t.Helper()
	var err error
	f.store, err = core.Open(ctx, data)
	if err != nil {
		t.Fatal("stopped packaged fixture could not reopen")
	}
	store := f.store
	t.Cleanup(func() { _ = store.Close() })
	f.e, err = New(store, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal("reopened packaged fixture engine unavailable")
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, store)
	f.handler = mux
}

type endedSettlementOutput struct {
	mu   sync.Mutex
	body []byte
	bad  bool
}

func (o *endedSettlementOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(p) > (64<<10)-len(o.body) {
		o.bad = true
	} else {
		o.body = append(o.body, p...)
	}
	return len(p), nil
}

type endedSettlementDaemon struct {
	command        *exec.Cmd
	stdout, stderr endedSettlementOutput
	done           chan error
	address        string
	once           sync.Once
}

func startEndedSettlementDaemon(t *testing.T, ctx context.Context, binary, data, instance string, env []string) *endedSettlementDaemon {
	t.Helper()
	d := &endedSettlementDaemon{command: exec.CommandContext(ctx, binary, "--data-dir", data, "--listen", "127.0.0.1:0"), done: make(chan error, 1)}
	d.command.Env, d.command.Stdout, d.command.Stderr, d.command.WaitDelay = env, &d.stdout, &d.stderr, time.Second
	if err := d.command.Start(); err != nil {
		t.Fatal("packaged settlement daemon could not start")
	}
	go func() { d.done <- d.command.Wait() }()
	t.Cleanup(func() { d.stop(t) })
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		d.stdout.mu.Lock()
		body := bytes.Clone(d.stdout.body)
		bad := d.stdout.bad
		d.stdout.mu.Unlock()
		if bytes.Contains(body, []byte{'\n'}) {
			var record struct {
				Event      string `json:"event"`
				Address    string `json:"address"`
				InstanceID string `json:"instance_id"`
			}
			host := ""
			if json.Unmarshal(bytes.TrimSpace(body), &record) == nil {
				host, _, _ = net.SplitHostPort(record.Address)
			}
			if bad || record.Event != "listening" || record.InstanceID != instance || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
				t.Fatal("packaged settlement readiness record invalid; raw output withheld")
			}
			d.address = "http://" + record.Address
			return d
		}
		select {
		case <-deadline.C:
			t.Fatal("packaged settlement readiness exceeded five seconds")
		case <-ctx.Done():
			t.Fatal("packaged settlement startup cancelled")
		case <-tick.C:
		}
	}
}

func (d *endedSettlementDaemon) request(t *testing.T, ctx context.Context, instance, token, key, path string, body []byte) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, d.address+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal("packaged settlement request unavailable")
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Instance", instance)
	r.Header.Set("Idempotency-Key", key)
	r.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(r)
	if err != nil {
		t.Fatal("packaged settlement HTTP request failed; raw diagnostics withheld")
	}
	defer response.Body.Close()
	out, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(out) > 64<<10 {
		t.Fatal("packaged settlement response exceeds bounded JSON")
	}
	return response.StatusCode, out
}

func (d *endedSettlementDaemon) stop(t *testing.T) {
	t.Helper()
	d.once.Do(func() {
		forced := runtime.GOOS == "windows"
		if forced {
			_ = d.command.Process.Kill()
		} else {
			_ = d.command.Process.Signal(os.Interrupt)
		}
		var err error
		select {
		case err = <-d.done:
		case <-time.After(5 * time.Second):
			_ = d.command.Process.Kill()
			select {
			case <-d.done:
			case <-time.After(time.Second):
				t.Error("owned settlement daemon did not join after kill")
			}
			t.Error("packaged settlement shutdown exceeded five seconds")
			return
		}
		var exit *exec.ExitError
		if err != nil && !(forced && errors.As(err, &exit) && exit.ExitCode() == 1) {
			t.Error("packaged settlement daemon exit failed; raw output withheld")
		}
		d.stderr.mu.Lock()
		bad := d.stderr.bad || len(d.stderr.body) != 0
		d.stderr.mu.Unlock()
		d.stdout.mu.Lock()
		bad = bad || d.stdout.bad || bytes.Count(d.stdout.body, []byte{'\n'}) != 1
		d.stdout.mu.Unlock()
		if bad {
			t.Error("packaged settlement daemon emitted unexpected output; raw output withheld")
		}
	})
}
