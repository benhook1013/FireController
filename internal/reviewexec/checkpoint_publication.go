package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type CheckpointPublicationRequest struct {
	EvidenceID       string `json:"evidence_id"`
	ExpectedRevision *int64 `json:"expected_revision"`
}
type CheckpointPublicationRecovery struct {
	ExpectedRevision *int64 `json:"expected_revision"`
	RetryUnposted    bool   `json:"retry_unposted,omitempty"`
}
type CheckpointPublication struct {
	ID                 string        `json:"id"`
	ProjectID          string        `json:"project_id"`
	EvidenceID         string        `json:"evidence_id"`
	State              string        `json:"state"`
	Revision           int64         `json:"revision"`
	Reason             string        `json:"reason"`
	SourceRevision     int64         `json:"source_revision"`
	Anchor             Anchor        `json:"anchor"`
	Counts             FindingCounts `json:"counts"`
	CreatedAt          string        `json:"created_at"`
	UpdatedAt          string        `json:"updated_at"`
	CommentID          int64         `json:"comment_id"`
	CommentURL         string        `json:"comment_url"`
	PublishedAt        string        `json:"published_at"`
	ProviderAuthorID   int64         `json:"provider_author_id"`
	ProviderAuthorKind string        `json:"provider_author_kind"`
}

// CheckpointProvider is a trusted scoped GitHub resource, never a client selector.
type CheckpointProvider interface {
	Repository(context.Context, int64) (reviewprovider.Repository, error)
	CurrentActor(context.Context) (reviewprovider.Author, error)
	PostIssueComment(context.Context, string, int64, string) (reviewprovider.IssueComment, error)
	IssueComments(context.Context, string, int64) ([]reviewprovider.IssueComment, error)
}
type checkpointIntent struct {
	Revision        int64                 `json:"revision"`
	CapabilityID    string                `json:"capability_id"`
	Fingerprint     string                `json:"fingerprint"`
	Provider        string                `json:"provider"`
	Account         string                `json:"account"`
	Repository      string                `json:"repository"`
	Actor           reviewprovider.Author `json:"actor"`
	EarliestPosting string                `json:"earliest_posting"`
	Body            string                `json:"body"`
}
type checkpointRecord struct {
	ConfirmationRepository string                           `json:"confirmation_repository,omitempty"`
	View                   CheckpointPublication            `json:"view"`
	Payload                reviewprovider.CheckpointPayload `json:"payload"`
	SemanticSHA256         string                           `json:"semantic_sha256"`
	CapabilityID           string                           `json:"capability_id"`
	Fingerprint            string                           `json:"fingerprint"`
	Provider               string                           `json:"provider"`
	Account                string                           `json:"account"`
	ActorID                string                           `json:"actor_id"`
	WorkerID               string                           `json:"worker_id"`
	Unposted               bool                             `json:"unposted"`
	Intent                 *checkpointIntent                `json:"intent,omitempty"`
}

func validPublicationID(id string) bool { return hostedObservationID(id, "review_publication_") }
func checkpointStorage() error {
	return core.Fail("storage_unavailable", "Stored checkpoint publication is unavailable.", 500)
}
func checkpointHold(reason string) error {
	return &core.Error{Code: "review_publication_held", Message: "Checkpoint publication is held; read its retained state.", Status: 409, Details: map[string]string{"reason": reason}}
}
func checkpointActive(state string) bool {
	switch state {
	case "queued", "preparing", "posting", "recover_queued", "recovering":
		return true
	}
	return false
}
func validCheckpointActor(a reviewprovider.Author) bool {
	return a.ID > 0 && (a.Kind == "User" || a.Kind == "Bot" || a.Kind == "Organization")
}
func checkpointTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil && !t.IsZero() && s == t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func checkpointSource(ctx context.Context, tx *sql.Tx, project, id, pubID string) (ReviewEvidence, ReviewAdjudication, reviewprovider.CheckpointPayload, error) {
	ev, state, err := sourceRouteState(ctx, tx, project, id)
	if err != nil {
		return ev, state, reviewprovider.CheckpointPayload{}, err
	}
	a, err := readAttempt(ctx, tx, project, ev.AttemptID)
	if err != nil {
		return ev, state, reviewprovider.CheckpointPayload{}, err
	}
	o, err := readOperation(ctx, tx, project, ev.OperationID)
	if err != nil {
		return ev, state, reviewprovider.CheckpointPayload{}, err
	}
	if a.State != "completed" || o.State != "completed" || !ev.Capture.Review.Complete || ev.Capture.Review.Outcome != "completed" || !state.Finalized || state.FrozenCounts == nil || state.FrozenCounts.Found != int64(len(ev.Capture.Review.Findings)) {
		return ev, state, reviewprovider.CheckpointPayload{}, checkpointHold("source_not_finalized")
	}
	counts := *state.FrozenCounts
	p := reviewprovider.CheckpointPayload{PublicationID: pubID, EvidenceID: id, RepositoryID: ev.Anchor.RepositoryID, PullNumber: ev.Anchor.PullNumber, Channel: evidenceChannel(ev.Capture), HeadSHA: ev.Anchor.CandidateSHA, BaseSHA: ev.Anchor.BaseSHA, BaseRef: ev.Anchor.BaseRef, ParentSHA: ev.Anchor.ParentSHA, MergeBaseSHA: ev.Anchor.MergeBaseSHA, PatchSHA256: ev.Anchor.PatchSHA256, CaptureSHA256: ev.Capture.CaptureSHA256, ParentPullNumber: ev.Anchor.ParentPullNumber, Found: counts.Found, Accepted: counts.Accepted, Routed: counts.Routed}
	if p.Channel == "cli" {
		n := int64(len(ev.Capture.Review.ReviewedFiles))
		p.Files = &n
	}
	start, se := time.Parse(time.RFC3339Nano, ev.Capture.StartedAt)
	end, ee := time.Parse(time.RFC3339Nano, ev.Capture.FinishedAt)
	if se == nil && ee == nil && !start.IsZero() && !end.Before(start) {
		n := int64(end.Sub(start) / time.Second)
		p.DurationSeconds = &n
	}
	if _, err = reviewprovider.RenderCheckpoint(p); err != nil {
		return ev, state, p, checkpointStorage()
	}
	return ev, state, p, nil
}
func (e *Engine) checkpointBinding(ctx context.Context, tx *sql.Tx, project string, repoID int64) (Capability, error) {
	var currentRepository int64
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&currentRepository); err != nil {
		return Capability{}, err
	}
	if currentRepository != repoID {
		return Capability{}, checkpointHold("repository_changed")
	}
	c, _, err := readConfiguration(ctx, tx, project)
	if err != nil {
		var f *core.Error
		if errors.As(err, &f) && f.Code == "reviews_unconfigured" {
			return Capability{}, checkpointHold("capability_unavailable")
		}
		return Capability{}, err
	}
	cap, ok := e.capabilities[c.CapabilityID]
	if !ok {
		return cap, checkpointHold("capability_unavailable")
	}
	return cap, nil
}
func checkpointCapacity(ctx context.Context, tx *sql.Tx, project string) error {
	var total, local int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM review_checkpoint_publications WHERE state IN ('queued','preparing','posting','recover_queued','recovering') LIMIT 64)`).Scan(&total); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM review_checkpoint_publications WHERE project_id=? AND state IN ('queued','preparing','posting','recover_queued','recovering') LIMIT 16)`, project).Scan(&local); err != nil {
		return err
	}
	if total >= 64 || local >= 16 {
		return core.Fail("overloaded", "Checkpoint publication capacity is occupied.", 503)
	}
	return nil
}
func decodeCheckpoint(body string) (checkpointRecord, error) {
	var r checkpointRecord
	if len(body) > 32<<10 || strictjson.Decode([]byte(body), &r) != nil || encoded(r) != body {
		return r, checkpointStorage()
	}
	v := r.View
	created, ok := checkpointTime(v.CreatedAt)
	updated, uok := checkpointTime(v.UpdatedAt)
	render, err := reviewprovider.RenderCheckpoint(r.Payload)
	if err != nil || render.SemanticSHA256 != r.SemanticSHA256 || !validPublicationID(v.ID) || !identifier.MatchString(v.ProjectID) || !hostedObservationID(v.EvidenceID, "review_evidence_") || v.ID != r.Payload.PublicationID || v.EvidenceID != r.Payload.EvidenceID || v.Revision < 1 || v.Revision == math.MaxInt64 || v.SourceRevision < 1 || !ok || !uok || updated.Before(created) || !validEvidenceAnchor(v.Anchor) || v.Anchor.RepositoryID != r.Payload.RepositoryID || v.Anchor.PullNumber != r.Payload.PullNumber || v.Anchor.CandidateSHA != r.Payload.HeadSHA || v.Anchor.BaseSHA != r.Payload.BaseSHA || v.Anchor.ParentSHA != r.Payload.ParentSHA || v.Anchor.ParentPullNumber != r.Payload.ParentPullNumber || v.Anchor.BaseRef != r.Payload.BaseRef || v.Anchor.MergeBaseSHA != r.Payload.MergeBaseSHA || v.Anchor.PatchSHA256 != r.Payload.PatchSHA256 || v.Counts != (FindingCounts{Found: r.Payload.Found, Accepted: r.Payload.Accepted, Routed: r.Payload.Routed}) || !identifier.MatchString(r.CapabilityID) || !hash.MatchString(r.Fingerprint) || !identifier.MatchString(r.Provider) || !identifier.MatchString(r.Account) || !identifier.MatchString(r.ActorID) || r.WorkerID != "" && !identifier.MatchString(r.WorkerID) || (v.Reason != "" && !identifier.MatchString(v.Reason)) || len(encoded(v))+1 > 16<<10 {
		return r, checkpointStorage()
	}
	if (v.State == "queued" || v.State == "preparing") && (!r.Unposted || r.Intent != nil) || (v.State == "posting" || v.State == "recovering" || v.State == "recover_queued" || v.State == "uncertain") && r.Unposted {
		return r, checkpointStorage()
	}
	if !checkpointActive(v.State) && v.State != "published" && v.State != "held" && v.State != "uncertain" {
		return r, checkpointStorage()
	}
	if r.Intent != nil {
		i := r.Intent
		_, tok := checkpointTime(i.EarliestPosting)
		if i.Revision < 1 || i.Revision > v.Revision || i.CapabilityID != r.CapabilityID || i.Fingerprint != r.Fingerprint || i.Provider != r.Provider || i.Account != r.Account || !repository.MatchString(i.Repository) || !validCheckpointActor(i.Actor) || !tok || i.Body != render.Body {
			return r, checkpointStorage()
		}
	}
	if (v.State == "posting" || v.State == "recovering" || v.State == "recover_queued" || v.State == "uncertain" || v.State == "published") && r.Intent == nil {
		return r, checkpointStorage()
	}
	if v.State == "published" {
		_, aok := checkpointTime(v.PublishedAt)
		if r.Intent == nil || !aok || v.CommentID <= 0 || !repository.MatchString(r.ConfirmationRepository) || v.CommentURL != checkpointURL(r.ConfirmationRepository, r.Payload.PullNumber, v.CommentID) || v.ProviderAuthorID != r.Intent.Actor.ID || v.ProviderAuthorKind != r.Intent.Actor.Kind || r.Unposted {
			return r, checkpointStorage()
		}
	} else if r.ConfirmationRepository != "" || v.CommentID != 0 || v.CommentURL != "" || v.PublishedAt != "" || v.ProviderAuthorID != 0 || v.ProviderAuthorKind != "" {
		return r, checkpointStorage()
	}
	return r, nil
}
func readCheckpoint(ctx context.Context, tx *sql.Tx, project, id string) (checkpointRecord, error) {
	var body, state, evidence, created string
	var revision int64
	var current, origin sql.NullString
	var historyActor, historyTime, originActor, originTime sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT p.body_json,p.state,p.revision,p.evidence_id,p.created_at,h.body_json,o.body_json,h.actor_id,h.created_at,o.actor_id,o.created_at FROM review_checkpoint_publications p LEFT JOIN review_checkpoint_revisions h ON h.publication_id=p.id AND h.revision=p.revision LEFT JOIN review_checkpoint_revisions o ON o.publication_id=p.id AND o.revision=1 WHERE p.project_id=? AND p.id=?`, project, id).Scan(&body, &state, &revision, &evidence, &created, &current, &origin, &historyActor, &historyTime, &originActor, &originTime)
	if errors.Is(err, sql.ErrNoRows) {
		return checkpointRecord{}, core.Fail("not_found", "The checkpoint publication is unavailable in this project.", 404)
	}
	if err != nil {
		return checkpointRecord{}, err
	}
	r, err := decodeCheckpoint(body)
	if err != nil {
		return r, err
	}
	o, err := decodeCheckpoint(origin.String)
	if err != nil {
		return r, err
	}
	if !historyActor.Valid || !historyTime.Valid || !originActor.Valid || !originTime.Valid || historyActor.String != r.ActorID || historyTime.String != r.View.UpdatedAt || originActor.String != o.ActorID || originTime.String != o.View.CreatedAt || !o.Unposted || o.View.UpdatedAt != o.View.CreatedAt || !current.Valid || !origin.Valid || current.String != body || r.View.ID != id || r.View.ProjectID != project || r.View.EvidenceID != evidence || r.View.State != state || r.View.Revision != revision || r.View.CreatedAt != created || o.View.Revision != 1 || o.View.State != "queued" || o.Intent != nil || encoded(o.Payload) != encoded(r.Payload) || o.View.Anchor != r.View.Anchor || o.View.SourceRevision != r.View.SourceRevision || o.View.CreatedAt != r.View.CreatedAt {
		return r, checkpointStorage()
	}
	if r.Intent != nil {
		var intent string
		if err := tx.QueryRowContext(ctx, `SELECT body_json FROM review_checkpoint_intents WHERE publication_id=? AND revision=?`, id, r.Intent.Revision).Scan(&intent); err != nil {
			return r, checkpointStorage()
		}
		if intent != encoded(r.Intent) {
			return r, checkpointStorage()
		}
	}
	return r, nil
}
func persistCheckpoint(ctx context.Context, tx *sql.Tx, r checkpointRecord, previous int64) error {
	if _, err := decodeCheckpoint(encoded(r)); err != nil {
		return err
	}
	v := r.View
	body := encoded(r)
	if previous == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_checkpoint_publications(id,project_id,evidence_id,state,revision,created_at,body_json) VALUES(?,?,?,?,?,?,?)`, v.ID, v.ProjectID, v.EvidenceID, v.State, v.Revision, v.CreatedAt, body); err != nil {
			return err
		}
	} else {
		res, err := tx.ExecContext(ctx, `UPDATE review_checkpoint_publications SET state=?,revision=?,body_json=? WHERE project_id=? AND id=? AND revision=?`, v.State, v.Revision, body, v.ProjectID, v.ID, previous)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return conflict()
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_checkpoint_revisions(publication_id,revision,actor_id,created_at,body_json) VALUES(?,?,?,?,?)`, v.ID, v.Revision, r.ActorID, v.UpdatedAt, body); err != nil {
		return err
	}
	return core.AppendEvent(ctx, tx, core.Scope{ProjectID: v.ProjectID, PrincipalID: r.ActorID, WorkerID: r.WorkerID}, "reviews.checkpoint_publication", v.ID, v.Revision, struct {
		State string `json:"state"`
	}{v.State})
}
func checkpointReceipt(r checkpointRecord) Receipt {
	return Receipt{ID: r.View.ID, State: "queued", Revision: r.View.Revision}
}
func (e *Engine) RequestCheckpointPublication(ctx context.Context, scope core.Scope, key string, in CheckpointPublicationRequest) (json.RawMessage, error) {
	if ctx == nil || e == nil || e.store == nil || !hostedObservationID(in.EvidenceID, "review_evidence_") || in.ExpectedRevision == nil || *in.ExpectedRevision < 0 {
		return nil, invalid()
	}
	revision := *in.ExpectedRevision
	in.ExpectedRevision = &revision
	raw, err := e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.checkpoint.publish", Target: in.EvidenceID, Key: key, Request: in}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM review_checkpoint_publications WHERE project_id=? AND evidence_id=?`, scope.ProjectID, in.EvidenceID).Scan(&id)
		existing := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if !existing {
			id = core.NewID("review_publication")
		}
		ev, state, payload, err := checkpointSource(ctx, tx, scope.ProjectID, in.EvidenceID, id)
		if err != nil {
			return nil, err
		}
		if state.Revision != revision {
			return nil, conflict()
		}
		if existing {
			r, err := readCheckpoint(ctx, tx, scope.ProjectID, id)
			if err != nil {
				return nil, err
			}
			if encoded(payload) != encoded(r.Payload) {
				return nil, checkpointStorage()
			}
			return checkpointReceipt(r), nil
		}
		cap, err := e.checkpointBinding(ctx, tx, scope.ProjectID, ev.Anchor.RepositoryID)
		if err != nil {
			return nil, err
		}
		if err = checkpointCapacity(ctx, tx, scope.ProjectID); err != nil {
			return nil, err
		}
		render, _ := reviewprovider.RenderCheckpoint(payload)
		now := sourceStamp()
		r := checkpointRecord{View: CheckpointPublication{ID: id, ProjectID: scope.ProjectID, EvidenceID: in.EvidenceID, State: "queued", Revision: 1, SourceRevision: state.Revision, Anchor: ev.Anchor, Counts: *state.FrozenCounts, CreatedAt: now, UpdatedAt: now}, Payload: payload, SemanticSHA256: render.SemanticSHA256, CapabilityID: cap.ID, Fingerprint: cap.Fingerprint, Provider: cap.Provider, Account: cap.Account, ActorID: scope.PrincipalID, WorkerID: scope.WorkerID, Unposted: true}
		if err = persistCheckpoint(ctx, tx, r, 0); err != nil {
			return nil, err
		}
		return checkpointReceipt(r), nil
	})
	if err == nil {
		e.wakeCheckpoints()
	}
	return raw, err
}
func (e *Engine) ReadCheckpointPublication(ctx context.Context, scope core.Scope, id string) (CheckpointPublication, error) {
	if ctx == nil || e == nil || e.store == nil || !validPublicationID(id) {
		return CheckpointPublication{}, invalid()
	}
	var r checkpointRecord
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		r, err = readCheckpoint(ctx, tx, scope.ProjectID, id)
		return err
	})
	if err != nil {
		return CheckpointPublication{}, err
	}
	return r.View, nil
}
func (e *Engine) RecoverCheckpointPublication(ctx context.Context, scope core.Scope, key, id string, in CheckpointPublicationRecovery) (json.RawMessage, error) {
	if ctx == nil || e == nil || e.store == nil || !validPublicationID(id) || in.ExpectedRevision == nil || *in.ExpectedRevision < 1 || *in.ExpectedRevision == math.MaxInt64 {
		return nil, invalid()
	}
	n := *in.ExpectedRevision
	in.ExpectedRevision = &n
	raw, err := e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.checkpoint.recover", Target: id, Key: key, Request: in}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		r, err := readCheckpoint(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return nil, err
		}
		if r.View.Revision != n {
			return nil, conflict()
		}
		if r.View.State == "published" || checkpointActive(r.View.State) {
			return checkpointReceipt(r), nil
		}
		if in.RetryUnposted {
			if !r.Unposted {
				return nil, checkpointHold("posting_outcome_unknown")
			}
			cap, err := e.checkpointBinding(ctx, tx, scope.ProjectID, r.Payload.RepositoryID)
			if err != nil {
				return nil, err
			}
			_, _, payload, err := checkpointSource(ctx, tx, scope.ProjectID, r.View.EvidenceID, id)
			if err != nil {
				return nil, err
			}
			if encoded(payload) != encoded(r.Payload) {
				return nil, checkpointStorage()
			}
			r.CapabilityID, r.Fingerprint, r.Provider, r.Account = cap.ID, cap.Fingerprint, cap.Provider, cap.Account
			r.ActorID, r.WorkerID = scope.PrincipalID, scope.WorkerID
			r.Intent = nil
			r.View.State = "queued"
		} else {
			if r.Unposted {
				return checkpointReceipt(r), nil
			}
			r.View.State = "recover_queued"
		}
		if err = checkpointCapacity(ctx, tx, scope.ProjectID); err != nil {
			return nil, err
		}
		r.View.Revision++
		r.View.Reason = ""
		r.View.UpdatedAt = sourceStamp()
		if err = persistCheckpoint(ctx, tx, r, n); err != nil {
			return nil, err
		}
		return checkpointReceipt(r), nil
	})
	if err == nil {
		e.wakeCheckpoints()
		e.notifyCheckpoint(id)
	}
	return raw, err
}
