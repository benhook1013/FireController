package reviewexec

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func checkpointHTTPFixture(t *testing.T) (*attemptFixture, string, string) {
	t.Helper()
	f, a, evidence, _ := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, evidence, 0)
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	return f, evidence, "/api/v1/projects/" + attemptProject + "/reviews/publications"
}

func TestCheckpointHTTPStrictScopeAndPendingReceipt(t *testing.T) {
	f, evidence, path := checkpointHTTPFixture(t)
	body := encoded(CheckpointPublicationRequest{EvidenceID: evidence, ExpectedRevision: adjudicationRevision(1)})
	zero := "wait=0"
	for _, bad := range []string{
		`{}`, `null`, `{"evidence_id":null,"expected_revision":1}`,
		`{"evidence_id":"` + evidence + `","expected_revision":null}`,
		`{"evidence_id":"` + evidence + `","expected_revision":-1}`,
		strings.Replace(body, `"expected_revision":1`, `"expected_revision":1,"expected_revision":1`, 1),
		strings.Replace(body, `"evidence_id"`, `"Evidence_id"`, 1),
		strings.TrimSuffix(body, "}") + `,"worker_id":"other"}`,
	} {
		w := sourceHTTPRequest(f, http.MethodPost, path, f.writer, "invalid", bad, &zero)
		if w.Code != 400 {
			t.Fatal("invalid publication request", bad, w.Code, w.Body.String())
		}
	}
	for _, prefer := range []string{"", "wait=-1", "wait=901", "wait=01", "wait=+1", "wait=2, wait=3", "respond-async"} {
		w := sourceHTTPRequest(f, http.MethodPost, path, f.writer, "invalid", body, &prefer)
		if w.Code != 400 {
			t.Fatal("invalid wait preference", prefer, w.Code)
		}
	}
	for _, tc := range []struct {
		token, path, key string
		want             int
	}{
		{"", path, "unauthorized", 401},
		{f.reader, path, "readonly", 403},
		{f.other, path, "other", 403},
		{f.writer, path + "?refresh=true", "query", 400},
		{f.writer, path, "", 400},
	} {
		w := sourceHTTPRequest(f, http.MethodPost, tc.path, tc.token, tc.key, body, &zero)
		if w.Code != tc.want {
			t.Fatal("authority/query/key boundary", w.Code, tc.want, w.Body.String())
		}
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_publications`) != 1 {
		t.Fatal("invalid requests changed the automatic publication")
	}
	w := sourceHTTPRequest(f, http.MethodPost, path, f.writer, "pending", body, &zero)
	var receipt Receipt
	var fields map[string]json.RawMessage
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || json.Unmarshal(w.Body.Bytes(), &fields) != nil || len(fields) != 3 || !validPublicationID(receipt.ID) || receipt.State != "queued" || receipt.Revision != 1 || w.Header().Get("Preference-Applied") != zero {
		t.Fatal("invalid compact pending receipt", w.Code, w.Body.String())
	}
	readPath := path + "/" + receipt.ID
	w = sourceHTTPRequest(f, http.MethodGet, readPath, f.reader, "", "", nil)
	var current CheckpointPublication
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &current) != nil || current.ID != receipt.ID || current.State != "queued" || current.SourceRevision != 1 || current.Counts != (FindingCounts{}) || current.CommentID != 0 || current.ProviderAuthorID != 0 {
		t.Fatal("stored compact publication read", w.Code, w.Body.String())
	}
	for _, private := range []string{"semantic_sha256", "capability_id", "fingerprint", "actor_id", "payload", f.writer} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatal("private publication state leaked", private)
		}
	}
	for _, tc := range []struct {
		path, token string
		want        int
	}{
		{readPath + "?refresh=true", f.reader, 400},
		{path + "/bad", f.reader, 400},
		{strings.Replace(readPath, attemptProject, otherAttemptProject, 1), f.other, 404},
	} {
		w := sourceHTTPRequest(f, http.MethodGet, tc.path, tc.token, "", "", nil)
		if w.Code != tc.want {
			t.Fatal("publication read boundary", tc.path, w.Code, w.Body.String())
		}
	}
	recovery := readPath + "/recovery"
	for _, bad := range []string{`{}`, `null`, `{"expected_revision":null}`, `{"expected_revision":1,"retry_unposted":null}`, `{"expected_revision":1,"retry_unposted":"true"}`, `{"expected_revision":1,"retry_unposted":false,"retry_unposted":false}`, `{"expected_revision":1,"other":true}`} {
		w := sourceHTTPRequest(f, http.MethodPost, recovery, f.writer, "bad-recovery", bad, &zero)
		if w.Code != 400 {
			t.Fatal("invalid recovery accepted", bad, w.Code, w.Body.String())
		}
	}
	w = sourceHTTPRequest(f, http.MethodPost, recovery, f.writer, "coalesce", `{"expected_revision":1}`, &zero)
	if w.Code != 202 || attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_publications`) != 1 {
		t.Fatal("active recovery did not coalesce", w.Code, w.Body.String())
	}
}

type checkpointHTTPProvider struct {
	reads atomic.Int32
	posts atomic.Int32
}

func (p *checkpointHTTPProvider) Repository(context.Context, int64) (reviewprovider.Repository, error) {
	p.reads.Add(1)
	return reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, nil
}
func (p *checkpointHTTPProvider) CurrentActor(context.Context) (reviewprovider.Author, error) {
	p.reads.Add(1)
	return reviewprovider.Author{ID: 71, Kind: "User", Login: "poster"}, nil
}
func (p *checkpointHTTPProvider) PostIssueComment(_ context.Context, _ string, _ int64, body string) (reviewprovider.IssueComment, error) {
	p.posts.Add(1)
	now := time.Now().UTC().Truncate(time.Second)
	return reviewprovider.IssueComment{ID: 91, Body: body, Author: reviewprovider.Author{ID: 71, Kind: "User", Login: "poster"}, CreatedAt: now, UpdatedAt: now}, nil
}
func (p *checkpointHTTPProvider) CheckpointComments(context.Context, string, int64, reviewprovider.Author) ([]reviewprovider.IssueComment, error) {
	p.reads.Add(1)
	return []reviewprovider.IssueComment{}, nil
}

func TestCheckpointHTTPFastResultAndTerminalReplayIgnoreWaitPreference(t *testing.T) {
	f, evidence, path := checkpointHTTPFixture(t)
	provider := &checkpointHTTPProvider{}
	ctx, cancel := context.WithCancel(t.Context())
	done, err := f.e.StartCheckpointPublications(ctx, map[string]CheckpointProvider{"test-capability": provider})
	if err != nil || done == nil {
		cancel()
		t.Fatal("publication worker startup", err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("publication worker shutdown", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("publication worker did not settle")
		}
	})
	body := encoded(CheckpointPublicationRequest{EvidenceID: evidence, ExpectedRevision: adjudicationRevision(1)})
	started := time.Now()
	w := sourceHTTPRequest(f, http.MethodPost, path, f.writer, "fast", body, nil)
	var current CheckpointPublication
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &current) != nil || current.State != "published" || current.CommentID != 91 || current.CommentURL != "https://github.com/owner/repo/pull/2#issuecomment-91" || time.Since(started) > time.Second || w.Body.Len() > 16<<10 {
		t.Fatal("quick publication did not return useful terminal facts", w.Code, w.Body.String())
	}
	reads := provider.reads.Load()
	events := attemptCount(t, f.store, `SELECT count(*) FROM events`)
	zero := "wait=0"
	for _, key := range []string{"fast", "new-key-same-evidence"} {
		w = sourceHTTPRequest(f, http.MethodPost, path, f.writer, key, body, &zero)
		if w.Code != 200 || provider.posts.Load() != 1 || provider.reads.Load() != reads || attemptCount(t, f.store, `SELECT count(*) FROM events`) != events {
			t.Fatal("terminal wait=0 replay repeated provider work or fabricated receipt", w.Code, w.Body.String())
		}
	}
	w = sourceHTTPRequest(f, http.MethodPost, path+"/"+current.ID+"/recovery", f.writer, "published-recovery", encoded(CheckpointPublicationRecovery{ExpectedRevision: &current.Revision}), &zero)
	if w.Code != 200 || provider.posts.Load() != 1 || provider.reads.Load() != reads {
		t.Fatal("published recovery did provider work", w.Code, w.Body.String())
	}
}
