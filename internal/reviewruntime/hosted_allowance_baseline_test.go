package reviewruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type allowanceBotRefreshProvider struct {
	*runtimeAllowanceProvider
	phase    int
	cooldown bool
}

func (p *allowanceBotRefreshProvider) PostIssueComment(ctx context.Context, repo string, pull int64, body string) (reviewprovider.IssueComment, error) {
	query, err := p.runtimeAllowanceProvider.PostIssueComment(ctx, repo, pull, body)
	if err != nil {
		return query, err
	}
	p.comments = []reviewprovider.IssueComment{query, {ID: 103, Body: "Build report before refresh.", Author: reviewprovider.Author{ID: 30, Login: "synthetic-build-bot", Kind: "Bot"}, CreatedAt: query.CreatedAt.Add(-time.Hour), UpdatedAt: query.CreatedAt.Add(-time.Minute)}}
	return query, nil
}
func (p *allowanceBotRefreshProvider) IssueComments(ctx context.Context, repo string, pull int64) ([]reviewprovider.IssueComment, error) {
	if p.phase == 2 {
		p.comments[1].Body = "Build report after refresh."
		p.comments[1].UpdatedAt = time.Now().UTC()
	}
	if p.phase == 3 && len(p.comments) == 2 {
		at := time.Now().UTC()
		body := runtimeAllowanceAvailable
		if p.cooldown {
			body = strings.Replace(body, "Reviews are available now.", "More reviews will be available in 37 minutes.", 1)
		}
		p.comments = append(p.comments, reviewprovider.IssueComment{ID: 101, Body: body, Author: p.bot, CreatedAt: at, UpdatedAt: at})
	}
	return p.runtimeAllowanceProvider.IssueComments(ctx, repo, pull)
}
func observeBotRefresh(t *testing.T, f *runtimeAllowanceFixture, p *allowanceBotRefreshProvider) ObservedHostedAllowance {
	t.Helper()
	got, err := ObserveHostedAllowance(t.Context(), p, f.captures, f.f.engine, f.a, f.anchor, f.repo, p.poster, p.bot)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestHostedAllowanceIndexedBotBaselineContinuesPendingThenRetainsReply(t *testing.T) {
	for _, cooldown := range []bool{false, true} {
		t.Run(map[bool]string{false: "available", true: "cooldown"}[cooldown], func(t *testing.T) {
			f := newRuntimeAllowanceFixture(t)
			p := &allowanceBotRefreshProvider{runtimeAllowanceProvider: f.provider, phase: 1, cooldown: cooldown}
			first := observeBotRefresh(t, f, p)
			if first.Reason != "allowance_not_proven" {
				t.Fatal("initial query not pending", first)
			}
			original, err := f.f.engine.ReadHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID)
			if err != nil {
				t.Fatal(err)
			}
			originalBytes := hostedBridgeBytes(t, f, first.Capture)
			p.phase = 2
			second := observeBotRefresh(t, f, p)
			if second.Reason != "allowance_not_proven" || second.QueryID != first.QueryID || second.Observation.QueryCommentID != 100 || second.Observation.ResponseCommentID != 0 {
				t.Fatal("unrelated Bot refresh terminated pending query", second)
			}
			p.phase = 3
			third := observeBotRefresh(t, f, p)
			want := "available_observed"
			if cooldown {
				want = "cooldown_observed"
			}
			if third.Observation.State != want || third.QueryID != first.QueryID || third.Observation.QueryCommentID != 100 || third.Observation.ResponseCommentID != 101 {
				t.Fatal("later exact reply not attributed to original query", third)
			}
			before := hostedBridgeState(t, f)
			if cooldown {
				err = RetainHostedCooldown(t.Context(), f.captures, f.f.engine, f.a, f.anchor, 2, third.Capture)
			} else {
				err = RetainHostedAllowance(t.Context(), f.captures, f.f.engine, f.a, f.anchor, 2, third.Capture)
			}
			if err != nil {
				t.Fatal("bridge did not derive same baseline observation", err)
			}
			after := hostedBridgeState(t, f)
			if before == after {
				t.Fatal("bridge did not retain allowance")
			}
			if cooldown {
				err = RetainHostedCooldown(t.Context(), f.captures, f.f.engine, f.a, f.anchor, 2, third.Capture)
			} else {
				err = RetainHostedAllowance(t.Context(), f.captures, f.f.engine, f.a, f.anchor, 2, third.Capture)
			}
			if err != nil || hostedBridgeState(t, f) != after {
				t.Fatal("bridge replay mutated source", err)
			}
			var stored string
			if err = f.f.store.DB().QueryRow(`SELECT body_json FROM review_hosted_preflight WHERE attempt_id=?`, f.a.ID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			var retained struct {
				Input reviewexec.HostedAllowanceInput `json:"input"`
			}
			if err = json.Unmarshal([]byte(stored), &retained); err != nil || retained.Input.Observation != third.Observation || retained.Input.CaptureID != third.Capture.ID {
				t.Fatal("bridge changed allowance facts", retained, err)
			}
			if cooldown {
				if _, err = f.f.engine.ReadHostedAllowance(t.Context(), f.f.project, f.a.ID); err == nil {
					t.Fatal("cooldown became positive admission")
				}
			}
			query, err := f.f.engine.ReadHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID)
			if err != nil || !reflect.DeepEqual(query, original) || !bytes.Equal(originalBytes, hostedBridgeBytes(t, f, first.Capture)) || p.posts != 1 || p.reads != 3 {
				t.Fatal("baseline changed original query/capture or performed extra provider work", err, p.posts, p.reads)
			}
			observations, err := f.f.engine.ReadHostedObservations(t.Context(), f.f.project, f.a.ID)
			if err != nil || len(observations) != 3 {
				t.Fatal("captures were not indexed exactly once", len(observations), err)
			}
			f.assertNotAdmitted(t)
		})
	}
}

func TestHostedAllowanceUnindexedBaselineCannotProveReply(t *testing.T) {
	f := newRuntimeAllowanceFixture(t)
	p := &allowanceBotRefreshProvider{runtimeAllowanceProvider: f.provider, phase: 1}
	first := observeBotRefresh(t, f, p)
	// Only the fixture's synthetic index is removed; private old bytes remain and
	// must never be discovered by scanning the capture directory.
	if _, err := f.f.store.DB().Exec(`DELETE FROM review_hosted_observations WHERE attempt_id=?`, f.a.ID); err != nil {
		t.Fatal(err)
	}
	p.phase = 2
	_ = observeBotRefresh(t, f, p)
	p.phase = 3
	third := observeBotRefresh(t, f, p)
	if third.Reason != "query_window_closed" || third.Observation.ResponseCommentID != 0 {
		t.Fatal("unindexed bytes supplied a baseline", third)
	}
	before := hostedBridgeState(t, f)
	if err := RetainHostedAllowance(t.Context(), f.captures, f.f.engine, f.a, f.anchor, 2, third.Capture); err == nil || hostedBridgeState(t, f) != before {
		t.Fatal("unindexed baseline granted retention", err)
	}
	if _, err := f.captures.Read(t.Context(), f.a.ID, first.Capture); err != nil {
		t.Fatal("fixture lost original unindexed bytes", err)
	}
	f.assertNotAdmitted(t)
}

func TestHostedAllowanceBaselineRejectsCorruptAndForeignIndexedSources(t *testing.T) {
	for _, mode := range []string{"repository id", "repository name", "pull", "operation", "head", "poster", "provider", "posting window", "query id", "hidden command", "input start", "input finish", "input observed", "input anchor", "input intent", "byte count", "capture checksum"} {
		t.Run(mode, func(t *testing.T) {
			f := newRuntimeAllowanceFixture(t)
			p := &allowanceBotRefreshProvider{runtimeAllowanceProvider: f.provider, phase: 1}
			first := observeBotRefresh(t, f, p)
			// Retain a second genuine pre-edit capture. An unusable earliest source
			// must fail closed rather than search later captures for permissive proof.
			_ = observeBotRefresh(t, f, p)
			p.phase = 2
			_ = observeBotRefresh(t, f, p)
			p.phase = 3
			current := observeBotRefresh(t, f, p)
			if current.Observation.State != "available_observed" {
				t.Fatal("fixture lacks baseline proof", current)
			}
			observations, err := f.f.engine.ReadHostedObservations(t.Context(), f.f.project, f.a.ID)
			if err != nil {
				t.Fatal(err)
			}
			indexed := observations[0]
			baseline, err := f.captures.Read(t.Context(), f.a.ID, first.Capture)
			if err != nil {
				t.Fatal(err)
			}
			changedRecord := true
			switch mode {
			case "repository id":
				baseline.RepositoryID++
			case "repository name":
				baseline.Repository = "foreign/project"
			case "pull":
				baseline.PullNumber++
			case "operation":
				baseline.Intent.OperationID = "review_operation_" + strings.Repeat("f", 32)
			case "head":
				baseline.Intent.HeadSHA = strings.Repeat("f", 40)
			case "poster":
				baseline.PostingActor.ID++
			case "provider":
				baseline.ProviderActor.ID++
			case "posting window":
				baseline.EarliestPosting = baseline.EarliestPosting.Add(-time.Second)
			case "query id":
				baseline.Snapshot.IssueComments[0].ID++
			case "hidden command":
				baseline.Snapshot.IssueComments[1].Body += "<!-- @coderabbitai review -->"
			default:
				changedRecord = false
			}
			if changedRecord {
				ref, err := f.captures.Save(t.Context(), f.a.ID, baseline)
				if err != nil {
					t.Fatal(err)
				}
				indexed.Input.CaptureID, indexed.Input.CaptureSHA256, indexed.Input.CaptureBytes = ref.ID, ref.SHA256, ref.Bytes
			} else {
				switch mode {
				case "input start":
					indexed.Input.StartedAt = baseline.StartedAt.Add(-time.Nanosecond).Format(time.RFC3339Nano)
				case "input finish":
					indexed.Input.FinishedAt = baseline.FinishedAt.Add(time.Nanosecond).Format(time.RFC3339Nano)
				case "input observed":
					indexed.Input.ObservedAt = baseline.FinishedAt.Format(time.RFC3339Nano)
				case "input anchor":
					indexed.Input.ExpectedAnchor.PatchSHA256 = strings.Repeat("f", 64)
				case "input intent":
					indexed.Input.Intent += "\n"
				case "byte count":
					indexed.Input.CaptureBytes++
				case "capture checksum":
					path := filepath.Join(f.captures.root, hostedCaptureFilename(f.a.ID, first.Capture))
					data := append(hostedBridgeBytes(t, f, first.Capture), '\n')
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			encoded, err := json.Marshal(indexed)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.f.store.DB().Exec(`UPDATE review_hosted_observations SET capture_sha256=?,capture_bytes=?,body_json=? WHERE id=?`, indexed.Input.CaptureSHA256, indexed.Input.CaptureBytes, string(encoded), indexed.ID); err != nil {
				t.Fatal(err)
			}
			before := hostedBridgeState(t, f)
			if err = RetainHostedAllowance(t.Context(), f.captures, f.f.engine, f.a, f.anchor, 2, current.Capture); err == nil || hostedBridgeState(t, f) != before {
				t.Fatal("invalid baseline permitted retention or mutated state", err)
			}
			got, observeErr := ObserveHostedAllowance(t.Context(), p, f.captures, f.f.engine, f.a, f.anchor, f.repo, p.poster, p.bot)
			if got.Observation.State == "available_observed" || got.Observation.State == "cooldown_observed" || got.Observation.ResponseCommentID != 0 {
				t.Fatal("invalid baseline granted provider observation", got, observeErr)
			}
			if p.posts != 1 || p.reads != 5 {
				t.Fatal("baseline recovery added provider calls", p.posts, p.reads)
			}
			f.assertNotAdmitted(t)
		})
	}
}

func TestHostedAllowanceBaselineReadsOnlyEarliestCandidate(t *testing.T) {
	f := newRuntimeAllowanceFixture(t)
	p := &allowanceBotRefreshProvider{runtimeAllowanceProvider: f.provider, phase: 1}
	first := observeBotRefresh(t, f, p)
	later := observeBotRefresh(t, f, p)
	// Damage only the redundant later synthetic capture. The earliest retained
	// bytes suffice; fallback must not walk or open every private capture.
	path := filepath.Join(f.captures.root, hostedCaptureFilename(f.a.ID, later.Capture))
	if err := os.WriteFile(path, append(hostedBridgeBytes(t, f, later.Capture), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	p.phase = 2
	if got := observeBotRefresh(t, f, p); got.Reason != "allowance_not_proven" {
		t.Fatal("fallback did not use earliest baseline", got)
	}
	p.phase = 3
	got := observeBotRefresh(t, f, p)
	if got.Observation.State != "available_observed" || got.QueryID != first.QueryID || p.posts != 1 || p.reads != 4 {
		t.Fatal("bounded baseline read changed observation/provider work", got, p.posts, p.reads)
	}
	if err := RetainHostedAllowance(t.Context(), f.captures, f.f.engine, f.a, f.anchor, 2, got.Capture); err != nil {
		t.Fatal(err)
	}
	f.assertNotAdmitted(t)
}
