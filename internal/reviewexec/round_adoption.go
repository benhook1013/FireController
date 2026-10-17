package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/roundadoptionmodel"
)

func normalizeRoundAdoption(p roundadoptionmodel.Prepared) (roundadoptionmodel.Prepared, []byte, error) {
	if p.Version != 1 || p.RepositoryID <= 0 || p.ExpectedRevision == nil || *p.ExpectedRevision != 0 || p.PullNumber < 1 || p.PullNumber > 2147483647 || !validChannel(p.Channel) || !validRoundSource(p.Source) || !validRoundSource(p.SourceKey) || len(p.SourceSHA256) != 64 || strings.Trim(p.SourceSHA256, "0123456789abcdef") != "" || p.MinimumRemaining < 0 || p.MaximumRemaining < 0 || p.MaximumRemaining > 1000 || p.MinimumRemaining > p.MaximumRemaining || p.InitialDry < 0 || p.InitialDry > 1000 || p.Taper < 1 || p.Taper > 1000 || p.InitialDry >= p.Taper && !p.Tapered {
		return p, nil, invalid()
	}
	zero := int64(0)
	p.ExpectedRevision = &zero
	body, err := json.Marshal(p)
	if err != nil || len(body) > 16384 {
		return p, nil, invalid()
	}
	return p, body, nil
}
func roundDigest(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func (e *Engine) roundAdoptionPreview(scope core.Scope, p roundadoptionmodel.Prepared, body []byte) roundadoptionmodel.Preview {
	binding, _ := json.Marshal(struct {
		Domain, Instance, Project, Principal, Worker string
		Body                                         json.RawMessage
	}{"firecontroller.round-adoption.preview.v1", e.store.InstanceID(), scope.ProjectID, scope.PrincipalID, scope.WorkerID, body})
	return roundadoptionmodel.Preview{ProjectID: scope.ProjectID, PullNumber: p.PullNumber, Channel: p.Channel, PayloadSHA256: roundDigest(body), PreviewSHA256: roundDigest(binding)}
}
func (e *Engine) admitRoundAdoption(ctx context.Context, tx *sql.Tx, scope core.Scope, p roundadoptionmodel.Prepared) error {
	var repository int64
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&repository); err != nil {
		return err
	}
	if repository != p.RepositoryID {
		return roundDenied("review_round_adoption_conflict", "The prepared rule does not match the destination repository.")
	}
	config, revision, err := readConfiguration(ctx, tx, scope.ProjectID)
	if err != nil {
		return err
	}
	if revision < 1 || !slices.Contains(config.Queue, p.PullNumber) || !slices.Contains(config.Channels, p.Channel) {
		return roundDenied("review_not_configured", "The selected PR and channel must be configured before adoption.")
	}
	c, err := readRoundRecord(ctx, tx, scope.ProjectID, p.PullNumber, p.Channel)
	if err != nil {
		return err
	}
	if c.Configured {
		return roundDenied("review_round_revision_conflict", "Adoption requires an unconfigured PR channel.")
	}
	var pending bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_operations WHERE project_id=? AND pull_number=? AND channel=? AND state IN `+roundPendingStates+`)`, scope.ProjectID, p.PullNumber, p.Channel).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return roundDenied("review_round_inflight", "Finish or safely settle the existing request before adoption.")
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_round_adoptions WHERE project_id=? AND source=? AND source_key=?)`, scope.ProjectID, p.Source, p.SourceKey).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return roundDenied("review_round_adoption_conflict", "The source rule has already been adopted.")
	}
	return nil
}
func (e *Engine) PreviewRoundAdoption(ctx context.Context, scope core.Scope, input roundadoptionmodel.Prepared) (roundadoptionmodel.Preview, error) {
	if !scope.Administrative() || scope.WorkerID == "" {
		return roundadoptionmodel.Preview{}, core.Fail("forbidden", "Administrative worker permission is required.", 403)
	}
	p, body, err := normalizeRoundAdoption(input)
	if err != nil {
		return roundadoptionmodel.Preview{}, err
	}
	err = e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error { return e.admitRoundAdoption(ctx, tx, scope, p) })
	if err != nil {
		return roundadoptionmodel.Preview{}, err
	}
	return e.roundAdoptionPreview(scope, p, body), nil
}
func (e *Engine) ApplyRoundAdoption(ctx context.Context, scope core.Scope, key string, input roundadoptionmodel.ApplyRequest) (json.RawMessage, error) {
	if !scope.Administrative() || scope.WorkerID == "" {
		return nil, core.Fail("forbidden", "Administrative worker permission is required.", 403)
	}
	p, body, err := normalizeRoundAdoption(input.Control)
	if err != nil {
		return nil, err
	}
	preview := e.roundAdoptionPreview(scope, p, body)
	if input.PreviewSHA256 != preview.PreviewSHA256 {
		return nil, core.Fail("import_preview_mismatch", "The prepared rule or destination differs from its preview.", 409)
	}
	input.Control = p
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.rounds.adopt", Target: encoded([]any{p.PullNumber, p.Channel}), Key: key, Request: input}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		if err := e.admitRoundAdoption(ctx, tx, scope, p); err != nil {
			return nil, err
		}
		at := stamp()
		c := roundRecord{RoundControl: RoundControl{ProjectID: scope.ProjectID, PullNumber: p.PullNumber, Channel: p.Channel, Configured: true, Revision: 1, Epoch: 1, Closed: p.Closed, Minimum: p.MinimumRemaining, Maximum: p.MaximumRemaining, Taper: p.Taper, InitialDry: p.InitialDry, InitiallyTapered: p.Tapered, Dry: p.InitialDry, Tapered: p.Tapered && p.MinimumRemaining == 0, OriginSource: p.Source, OriginKey: p.SourceKey, OriginSHA256: p.SourceSHA256, ActorID: scope.PrincipalID, CreatedAt: at, UpdatedAt: at}, grantRevision: 1}
		if !validRoundControl(c.RoundControl) {
			return nil, roundUnavailable()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_round_changes(project_id,pull_number,channel,revision,epoch,action,body_json,worker_id) VALUES(?,?,?,1,1,'grant',?,?)`, scope.ProjectID, p.PullNumber, p.Channel, encoded(c.RoundControl), scope.WorkerID); err != nil {
			return nil, err
		}
		if err := storeRoundRecord(ctx, tx, c); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_round_adoptions(project_id,pull_number,channel,grant_revision,epoch,source,source_key,source_sha256,prepared_json) VALUES(?,?,?,1,1,?,?,?,?)`, scope.ProjectID, p.PullNumber, p.Channel, p.Source, p.SourceKey, p.SourceSHA256, string(body)); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, "reviews.rounds_configured", scope.ProjectID, 1, map[string]any{"pull_number": p.PullNumber, "channel": p.Channel, "revision": 1}); err != nil {
			return nil, err
		}
		return RoundControlReceipt{PullNumber: p.PullNumber, Channel: p.Channel, Revision: 1, Epoch: 1, Outcome: "adopted"}, nil
	})
}
