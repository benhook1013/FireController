package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type RoundControlRequest struct {
	ExpectedRevision *int64 `json:"expected_revision"`
	Action           string `json:"action"`
	Minimum          *int64 `json:"minimum,omitempty"`
	Maximum          *int64 `json:"maximum,omitempty"`
}

type RoundControl struct {
	InitialDry       int64  `json:"initial_dry,omitempty"`
	InitiallyTapered bool   `json:"initially_tapered,omitempty"`
	OriginSource     string `json:"origin_source,omitempty"`
	OriginKey        string `json:"origin_key,omitempty"`
	OriginSHA256     string `json:"origin_sha256,omitempty"`
	ProjectID        string `json:"project_id"`
	PullNumber       int64  `json:"pull_number"`
	Channel          string `json:"channel"`
	Configured       bool   `json:"configured"`
	Revision         int64  `json:"revision"`
	Epoch            int64  `json:"epoch"`
	Closed           bool   `json:"closed"`
	Minimum          int64  `json:"minimum"`
	Maximum          int64  `json:"maximum"`
	MaximumUnbounded bool   `json:"maximum_unbounded,omitempty"`
	Taper            int64  `json:"taper"`
	Used             int64  `json:"used"`
	Dry              int64  `json:"dry"`
	Tapered          bool   `json:"tapered"`
	ActorID          string `json:"actor_id"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

type RoundControlReceipt struct {
	PullNumber int64  `json:"pull_number"`
	Channel    string `json:"channel"`
	Revision   int64  `json:"revision"`
	Epoch      int64  `json:"epoch"`
	Outcome    string `json:"outcome"`
}

type roundRecord struct {
	RoundControl
	grantRevision int64
}

const roundColumnsV1 = `project_id,pull_number,channel,revision,epoch,grant_revision,closed,minimum,maximum,taper,used,dry,tapered,actor_id,created_at,updated_at`
const roundColumnsV2 = roundColumnsV1 + `,initial_dry,initially_tapered,origin_source,origin_key,origin_sha256`
const roundColumns = roundColumnsV2 + `,maximum_unbounded`
const roundSafeInteger int64 = 9007199254740991
const roundPendingStates = `('queued','preparing','posting','active','uncertain')`

func roundUnavailable() error {
	return core.Fail("storage_unavailable", "Stored review round controls are unavailable.", 500)
}
func roundDenied(code, message string) error { return core.Fail(code, message, 409) }
func roundScope(project string, pull int64, channel string) bool {
	return identifier.MatchString(project) && pull >= 1 && pull <= 2147483647 && validChannel(channel)
}
func roundTaper(channel string) int64 {
	if channel == "hosted" {
		return 1
	}
	return 3
}
func roundDone(c RoundControl) bool {
	return c.Configured && (!c.MaximumUnbounded && c.Used >= c.Maximum || c.Tapered)
}
func roundTime(value string) bool {
	at, err := time.Parse(time.RFC3339Nano, value)
	return len(value) <= 64 && err == nil && !at.IsZero()
}
func validRoundOrigin(c RoundControl) bool {
	return validRoundSource(c.OriginSource) && validRoundSource(c.OriginKey) && len(c.OriginSHA256) == 64 && strings.Trim(c.OriginSHA256, "0123456789abcdef") == ""
}
func validRoundSource(s string) bool {
	return len(s) <= 200 && utf8.ValidString(s) && s != "" && s == strings.TrimSpace(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}
func hasRoundOrigin(c RoundControl) bool {
	return c.OriginSource != "" || c.OriginKey != "" || c.OriginSHA256 != ""
}
func validRoundControl(c RoundControl) bool {
	origin := hasRoundOrigin(c)
	baseline := !c.MaximumUnbounded && c.Maximum >= 1 && c.Taper == roundTaper(c.Channel) && c.InitialDry == 0 && !c.InitiallyTapered
	if origin {
		baseline = validRoundOrigin(c) && c.Maximum >= 0 && c.Taper >= 1 && c.Taper <= 1000 && c.InitialDry >= 0 && c.InitialDry <= 1000 && (c.InitialDry < c.Taper || c.InitiallyTapered)
	}
	return baseline && roundScope(c.ProjectID, c.PullNumber, c.Channel) && c.Configured && c.Revision >= 1 && c.Epoch >= 1 && c.Epoch <= c.Revision && c.Minimum >= 0 && c.Used >= 0 && c.Used <= roundSafeInteger && c.Dry >= 0 && c.Dry <= roundSafeInteger && c.Dry <= c.Used+c.InitialDry && (c.MaximumUnbounded && c.Maximum == 0 && c.Minimum <= roundSafeInteger || !c.MaximumUnbounded && c.Minimum <= c.Maximum && c.Maximum <= 1000 && c.Used <= c.Maximum) && (!c.Tapered || c.Used >= c.Minimum && (c.InitiallyTapered || c.Dry >= c.Taper)) && (!c.InitiallyTapered || c.Used < c.Minimum || c.Tapered) && identifier.MatchString(c.ActorID) && roundTime(c.CreatedAt) && roundTime(c.UpdatedAt)
}

func readRoundRecord(ctx context.Context, q EvidenceQuery, project string, pull int64, channel string) (roundRecord, error) {
	return readRoundRecordVersion(ctx, q, project, pull, channel, 3)
}
func readRoundRecordVersion(ctx context.Context, q EvidenceQuery, project string, pull int64, channel string, version int) (roundRecord, error) {
	c := roundRecord{RoundControl: RoundControl{ProjectID: project, PullNumber: pull, Channel: channel}}
	if !roundScope(project, pull, channel) {
		return c, invalid()
	}
	columns := roundColumns
	if version == 1 {
		columns = roundColumnsV1 + `,0,0,'','','',0`
	} else if version == 2 {
		columns = roundColumnsV2 + `,0`
	}
	err := q.QueryRowContext(ctx, `SELECT `+columns+` FROM review_round_controls WHERE project_id=? AND pull_number=? AND channel=?`, project, pull, channel).Scan(&c.ProjectID, &c.PullNumber, &c.Channel, &c.Revision, &c.Epoch, &c.grantRevision, &c.Closed, &c.Minimum, &c.Maximum, &c.Taper, &c.Used, &c.Dry, &c.Tapered, &c.ActorID, &c.CreatedAt, &c.UpdatedAt, &c.InitialDry, &c.InitiallyTapered, &c.OriginSource, &c.OriginKey, &c.OriginSHA256, &c.MaximumUnbounded)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	c.Configured = true
	if !validRoundControl(c.RoundControl) || c.grantRevision < 1 || c.grantRevision > c.Revision {
		return roundRecord{}, roundUnavailable()
	}
	return c, nil
}

// readRoundControl is local transactional policy state, never capture coverage.
func readRoundControl(ctx context.Context, q EvidenceQuery, project string, pull int64, channel string) (RoundControl, error) {
	c, err := readRoundRecord(ctx, q, project, pull, channel)
	return c.RoundControl, err
}

func (e *Engine) ReadRoundControl(ctx context.Context, scope core.Scope, pull int64, channel string) (RoundControl, error) {
	if !roundScope(scope.ProjectID, pull, channel) {
		return RoundControl{}, invalid()
	}
	var out RoundControl
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		out, err = readRoundControl(ctx, tx, scope.ProjectID, pull, channel)
		return err
	})
	return out, err
}

func (e *Engine) ConfigureRoundControl(ctx context.Context, scope core.Scope, key string, pull int64, channel string, input RoundControlRequest) (json.RawMessage, error) {
	// Store.Mutate rechecks current project write permission transactionally,
	// including exact-key receipt replay before the callback is considered.
	if !roundScope(scope.ProjectID, pull, channel) || input.ExpectedRevision == nil || *input.ExpectedRevision < 0 || *input.ExpectedRevision == math.MaxInt64 {
		return nil, invalid()
	}
	previous := *input.ExpectedRevision
	input.ExpectedRevision = &previous
	if input.Action == "grant" {
		if input.Minimum == nil || input.Maximum == nil {
			return nil, invalid()
		}
		minimum, maximum := *input.Minimum, *input.Maximum
		if minimum < 0 || maximum < 1 || maximum > 1000 || minimum > maximum {
			return nil, invalid()
		}
		input.Minimum, input.Maximum = &minimum, &maximum
	} else if input.Action != "close" || input.Minimum != nil || input.Maximum != nil {
		return nil, invalid()
	}
	target := fmt.Sprintf("%d/%s", pull, channel)
	return e.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.rounds.configure", Target: target, Key: key, Request: input}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		c, err := readRoundRecord(ctx, tx, scope.ProjectID, pull, channel)
		if err != nil {
			return nil, err
		}
		if c.Revision != previous {
			return nil, &core.Error{Code: "review_round_revision_conflict", Message: "Reload the current review round control before changing it.", Status: 409, Details: map[string]int64{"current_revision": c.Revision}}
		}
		if c.Revision == math.MaxInt64 || input.Action == "grant" && c.Epoch == math.MaxInt64 {
			return nil, roundDenied("review_round_revision_exhausted", "The retained round control cannot advance.")
		}
		at := stamp()
		if !c.Configured {
			c.Configured = true
			c.CreatedAt = at
			c.Taper = roundTaper(channel)
		}
		c.Revision++
		c.ActorID = scope.PrincipalID
		c.UpdatedAt = at
		if input.Action == "grant" {
			var pending bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_operations WHERE project_id=? AND pull_number=? AND channel=? AND state IN `+roundPendingStates+`)`, scope.ProjectID, pull, channel).Scan(&pending); err != nil {
				return nil, err
			}
			if pending {
				return nil, roundDenied("review_round_inflight", "Finish or safely settle the existing request before granting more rounds.")
			}
			c.Epoch++
			c.grantRevision = c.Revision
			c.Closed = false
			c.Minimum, c.Maximum = *input.Minimum, *input.Maximum
			c.MaximumUnbounded = false
			c.Used, c.Dry, c.Tapered = 0, 0, false
			c.InitialDry, c.InitiallyTapered, c.OriginSource, c.OriginKey, c.OriginSHA256 = 0, false, "", "", ""
			c.Taper = roundTaper(channel)
		} else {
			if c.Epoch == 0 {
				c.Epoch = 1
				c.grantRevision = c.Revision
				c.Minimum, c.Maximum = 0, 1
			}
			c.Closed = true
		}
		if !validRoundControl(c.RoundControl) {
			return nil, roundUnavailable()
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_round_changes(project_id,pull_number,channel,revision,epoch,action,body_json,worker_id) VALUES(?,?,?,?,?,?,?,?)`, c.ProjectID, c.PullNumber, c.Channel, c.Revision, c.Epoch, input.Action, encoded(c.RoundControl), scope.WorkerID); err != nil {
			return nil, err
		}
		if err = storeRoundRecord(ctx, tx, c); err != nil {
			return nil, err
		}

		if err = core.AppendEvent(ctx, tx, scope, "reviews.rounds_configured", target, c.Revision, map[string]any{"pull_number": pull, "channel": channel, "revision": c.Revision}); err != nil {
			return nil, err
		}
		outcome := "granted"
		if input.Action == "close" {
			outcome = "closed"
		}
		return RoundControlReceipt{PullNumber: pull, Channel: channel, Revision: c.Revision, Epoch: c.Epoch, Outcome: outcome}, nil
	})
}

func checkRoundOpen(c RoundControl) error {
	if c.Closed {
		return roundDenied("review_round_closed", "This PR review channel is explicitly closed.")
	}
	if roundDone(c) {
		return roundDenied("review_round_complete", "This PR review channel has completed its additional rounds.")
	}
	if c.Used >= roundSafeInteger || c.Dry >= roundSafeInteger {
		return roundDenied("review_round_capacity", "The retained round counters cannot advance safely.")
	}
	return nil
}

// Subtraction keeps in-flight accounting within the interoperable integer bound.
func roundHasCapacity(c RoundControl, occupied int64) bool {
	return occupied >= 0 && c.Used >= 0 && c.Used < roundSafeInteger && occupied < roundSafeInteger-c.Used && (c.MaximumUnbounded || occupied < c.Maximum-c.Used)
}

func roundOccupied(ctx context.Context, tx *sql.Tx, c roundRecord, excluding string) (int64, error) {
	var occupied int64
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM review_round_operation_bindings b JOIN review_operations o ON o.id=b.operation_id WHERE b.project_id=? AND b.pull_number=? AND b.channel=? AND b.epoch=? AND b.operation_id<>? AND o.state IN `+roundPendingStates, c.ProjectID, c.PullNumber, c.Channel, c.Epoch, excluding).Scan(&occupied)
	if err != nil {
		return 0, err
	}
	return occupied, nil
}

// bindRoundControl runs after inserting the new queued operation, inside its
// acceptance transaction. Unconfigured channels retain their existing policy.
func bindRoundControl(ctx context.Context, tx *sql.Tx, operation Operation) error {
	c, err := readRoundRecord(ctx, tx, operation.ProjectID, operation.PullNumber, operation.Channel)
	if err != nil {
		return err
	}
	if !c.Configured {
		return nil
	}
	if err = checkRoundOpen(c.RoundControl); err != nil {
		return err
	}
	actual, err := readOperation(ctx, tx, operation.ProjectID, operation.ID)
	if err != nil {
		return err
	}
	if actual.PullNumber != operation.PullNumber || actual.Channel != operation.Channel || actual.State != "queued" {
		return roundUnavailable()
	}
	var epoch, grant int64
	err = tx.QueryRowContext(ctx, `SELECT epoch,grant_revision FROM review_round_operation_bindings WHERE operation_id=?`, operation.ID).Scan(&epoch, &grant)
	if err == nil {
		if epoch != c.Epoch || grant != c.grantRevision {
			return roundDenied("review_round_changed", "The request belongs to a different round grant.")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	occupied, err := roundOccupied(ctx, tx, c, operation.ID)
	if err != nil {
		return err
	}
	if !roundHasCapacity(c.RoundControl, occupied) {
		return roundDenied("review_round_capacity", "Existing requests occupy the remaining round allowance.")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO review_round_operation_bindings(operation_id,project_id,pull_number,channel,epoch,grant_revision) VALUES(?,?,?,?,?,?)`, operation.ID, operation.ProjectID, operation.PullNumber, operation.Channel, c.Epoch, c.grantRevision)
	return err
}

// admitRoundControl rechecks a bound request immediately before preparation or
// provider dispatch. true means explicit configured discovery is still needed.
func admitRoundControl(ctx context.Context, tx *sql.Tx, operation Operation) (bool, error) {
	c, err := readRoundRecord(ctx, tx, operation.ProjectID, operation.PullNumber, operation.Channel)
	if err != nil {
		return false, err
	}
	var project, channel string
	var pull, epoch, grant int64
	err = tx.QueryRowContext(ctx, `SELECT project_id,pull_number,channel,epoch,grant_revision FROM review_round_operation_bindings WHERE operation_id=?`, operation.ID).Scan(&project, &pull, &channel, &epoch, &grant)
	if errors.Is(err, sql.ErrNoRows) {
		if !c.Configured {
			return false, nil
		}
		return false, roundDenied("review_round_changed", "The request is not bound to the current round grant.")
	}
	if err != nil {
		return false, err
	}
	if !c.Configured || project != operation.ProjectID || pull != operation.PullNumber || channel != operation.Channel {
		return false, roundUnavailable()
	}
	if epoch != c.Epoch || grant != c.grantRevision {
		return false, roundDenied("review_round_changed", "The request belongs to a different round grant.")
	}
	if err = checkRoundOpen(c.RoundControl); err != nil {
		return false, err
	}
	occupied, err := roundOccupied(ctx, tx, c, operation.ID)
	if err != nil {
		return false, err
	}
	if !roundHasCapacity(c.RoundControl, occupied) {
		return false, roundDenied("review_round_capacity", "Existing requests occupy the remaining round allowance.")
	}
	return true, nil
}

func readRoundGrant(ctx context.Context, q EvidenceQuery, project string, pull int64, channel string, revision, epoch int64) (RoundControl, error) {
	var action, body string
	if err := q.QueryRowContext(ctx, `SELECT action,body_json FROM review_round_changes WHERE project_id=? AND pull_number=? AND channel=? AND revision=? AND epoch=?`, project, pull, channel, revision, epoch).Scan(&action, &body); err != nil {
		return RoundControl{}, err
	}
	var c RoundControl
	if len(body) > 4096 || strictjson.Decode([]byte(body), &c) != nil || !validRoundControl(c) || c.ProjectID != project || c.PullNumber != pull || c.Channel != channel || c.Revision != revision || c.Epoch != epoch || action != "grant" || c.Closed || hasRoundOrigin(c) && roundDone(c) || c.Used != 0 || c.Dry != c.InitialDry || c.Tapered != (c.InitiallyTapered && c.Minimum == 0) {
		return RoundControl{}, roundUnavailable()
	}
	return c, nil
}

// completeRoundControl credits only a bound native completed capture with
// finalized frozen source counts. Pending accepted fixes do not erase a round.
// The caller already owns the evidence mutation; it must roll back on error.
func completeRoundControl(ctx context.Context, tx *sql.Tx, project, operationID string) error {
	var pull, epoch, grant int64
	var channel string
	err := tx.QueryRowContext(ctx, `SELECT pull_number,channel,epoch,grant_revision FROM review_round_operation_bindings WHERE operation_id=? AND project_id=?`, operationID, project).Scan(&pull, &channel, &epoch, &grant)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	operation, err := readOperation(ctx, tx, project, operationID)
	if err != nil {
		return err
	}
	if operation.PullNumber != pull || operation.Channel != channel {
		return roundUnavailable()
	}
	if operation.State != "completed" {
		return nil
	}
	proof, err := deriveComparisonProof(ctx, tx, project, operationID)
	if err != nil {
		return err
	}
	if proof.evidence == "" {
		return nil
	}
	evidence, state, err := loadReviewAdjudication(ctx, tx, project, proof.evidence)
	if err != nil {
		return err
	}
	if !state.Finalized || state.FrozenCounts == nil {
		return nil
	}
	if evidence.OperationID != operationID || evidence.AttemptID != operation.AttemptID || !evidence.Capture.Review.Complete || evidence.Capture.Review.Outcome != "completed" {
		return nil
	}
	var oldEvidence string
	var oldEpoch, oldAccepted int64
	err = tx.QueryRowContext(ctx, `SELECT evidence_id,epoch,accepted FROM review_round_completions WHERE operation_id=?`, operationID).Scan(&oldEvidence, &oldEpoch, &oldAccepted)
	if err == nil {
		if oldEvidence != proof.evidence || oldEpoch != epoch || oldAccepted != state.FrozenCounts.Accepted {
			return roundUnavailable()
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	original, err := readRoundGrant(ctx, tx, project, pull, channel, grant, epoch)
	if err != nil {
		return err
	}
	var used int64
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM review_round_completions WHERE project_id=? AND pull_number=? AND channel=? AND epoch=?`, project, pull, channel, epoch).Scan(&used); err != nil {
		return err
	}
	if used >= roundSafeInteger || !original.MaximumUnbounded && used >= original.Maximum {
		return roundUnavailable()
	}
	at := stamp()
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_round_completions(operation_id,evidence_id,project_id,pull_number,channel,epoch,ordinal,accepted,completed_at) VALUES(?,?,?,?,?,?,?,?,?)`, operationID, proof.evidence, project, pull, channel, epoch, used+1, state.FrozenCounts.Accepted, at); err != nil {
		return err
	}
	c, err := readRoundRecord(ctx, tx, project, pull, channel)
	if err != nil {
		return err
	}
	if !c.Configured {
		return roundUnavailable()
	}
	if c.Epoch < epoch {
		return roundUnavailable()
	}
	if c.Epoch > epoch {
		return nil
	}
	if c.grantRevision != grant || c.Used != used || (!c.MaximumUnbounded && c.Used >= c.Maximum) || c.Used >= roundSafeInteger {
		return roundUnavailable()
	}
	done := roundDone(c.RoundControl)
	if !done && state.FrozenCounts.Accepted == 0 && c.Dry >= roundSafeInteger {
		return roundUnavailable()
	}
	c.Used++
	if !done {
		if state.FrozenCounts.Accepted == 0 {
			c.Dry++
		} else {
			c.Dry = 0
		}
		if c.Used >= c.Minimum && (c.InitiallyTapered || c.Dry >= c.Taper) {
			c.Tapered = true
		}
	}
	if !validRoundControl(c.RoundControl) {
		return roundUnavailable()
	}
	_, err = tx.ExecContext(ctx, `UPDATE review_round_controls SET used=?,dry=?,tapered=?,updated_at=? WHERE project_id=? AND pull_number=? AND channel=? AND epoch=?`, c.Used, c.Dry, c.Tapered, at, project, pull, channel, epoch)
	return err
}

func storeRoundRecord(ctx context.Context, tx *sql.Tx, c roundRecord) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO review_round_controls(`+roundColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(project_id,pull_number,channel) DO UPDATE SET revision=excluded.revision,epoch=excluded.epoch,grant_revision=excluded.grant_revision,closed=excluded.closed,minimum=excluded.minimum,maximum=excluded.maximum,taper=excluded.taper,used=excluded.used,dry=excluded.dry,tapered=excluded.tapered,actor_id=excluded.actor_id,updated_at=excluded.updated_at,initial_dry=excluded.initial_dry,initially_tapered=excluded.initially_tapered,origin_source=excluded.origin_source,origin_key=excluded.origin_key,origin_sha256=excluded.origin_sha256,maximum_unbounded=excluded.maximum_unbounded`, c.ProjectID, c.PullNumber, c.Channel, c.Revision, c.Epoch, c.grantRevision, c.Closed, c.Minimum, c.Maximum, c.Taper, c.Used, c.Dry, c.Tapered, c.ActorID, c.CreatedAt, c.UpdatedAt, c.InitialDry, c.InitiallyTapered, c.OriginSource, c.OriginKey, c.OriginSHA256, c.MaximumUnbounded)
	return err
}
