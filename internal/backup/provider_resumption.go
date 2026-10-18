package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type ProviderResumptionRequest struct {
	ExpectedInstanceID     string `json:"expected_instance_id"`
	FencedPredecessorID    string `json:"fenced_predecessor_id"`
	ProviderEffectsSettled bool   `json:"provider_effects_settled"`
	AbandonRetainedWork    bool   `json:"abandon_retained_work"`
	Reason                 string `json:"reason"`
	EvidenceReference      string `json:"evidence_reference"`
}

type ProviderResumptionSummary struct {
	InstanceID       string                         `json:"instance_id"`
	SourceInstanceID string                         `json:"source_instance_id"`
	BundleID         string                         `json:"bundle_id"`
	State            string                         `json:"state"`
	ProvidersHeld    bool                           `json:"providers_held"`
	ReleasedAt       string                         `json:"released_at"`
	Counts           reviewexec.RestoredWorkSummary `json:"counts"`
}

type providerRelease struct {
	Version     int                            `json:"version"`
	Request     ProviderResumptionRequest      `json:"request"`
	BundleID    string                         `json:"bundle_id"`
	ActivatedAt string                         `json:"activated_at"`
	ReleasedAt  string                         `json:"released_at"`
	Counts      reviewexec.RestoredWorkSummary `json:"counts"`
}

func validResumptionRequest(in ProviderResumptionRequest) bool {
	if !instanceID.MatchString(in.ExpectedInstanceID) || !instanceID.MatchString(in.FencedPredecessorID) || in.ExpectedInstanceID == in.FencedPredecessorID || !in.ProviderEffectsSettled || !in.AbandonRetainedWork {
		return false
	}
	for _, value := range []string{in.Reason, in.EvidenceReference} {
		if strings.TrimSpace(value) == "" || len(value) > 256 || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return false
		}
	}
	return true
}

func readProviderRelease(fields map[string]string, status RecoveryStatus) (providerRelease, error) {
	var release providerRelease
	body := fields["restore:provider_release"]
	if len(body) == 0 || len(body) > maxRecoveryHistoryBytes || strictjson.Decode([]byte(body), &release) != nil {
		return release, invalid()
	}
	canonical, err := json.Marshal(release)
	activation, activationOK := canonicalTime(release.ActivatedAt)
	at, atOK := canonicalTime(release.ReleasedAt)
	if err != nil || string(canonical) != body || release.Version != 1 || !validResumptionRequest(release.Request) || release.Request.ExpectedInstanceID != status.InstanceID || release.Request.FencedPredecessorID != status.SourceInstanceID || release.BundleID != status.BundleID || release.ActivatedAt != status.ActivatedAt || !activationOK || !atOK || at.Before(activation) {
		return release, invalid()
	}
	for _, count := range []int64{release.Counts.QueuedOperations, release.Counts.Attempts, release.Counts.SourceRefreshes, release.Counts.Publications, release.Counts.PausedProjects} {
		if count < 0 || count > 10000 {
			return release, invalid()
		}
	}
	archived, err := json.Marshal(fields)
	if err != nil || len(archived) > maxRecoveryHistoryBytes {
		return release, invalid()
	}
	return release, nil
}

func resumptionSummary(status RecoveryStatus, release providerRelease) ProviderResumptionSummary {
	return ProviderResumptionSummary{InstanceID: status.InstanceID, SourceInstanceID: status.SourceInstanceID, BundleID: status.BundleID, State: status.State, ReleasedAt: release.ReleasedAt, Counts: release.Counts}
}

// Only pre-write validation failures use this normalization. In particular,
// commit failures retain their uncertain-outcome diagnostic, and successful
// commits must never be reported as interrupted if cancellation arrives later.
func providerResumptionValidationError(ctx context.Context, err error) error {
	if err != nil && (ctx != nil && ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return core.Fail("restore_reconciliation_interrupted", "Restore reconciliation was interrupted; no provider release was recorded. Retry the exact reconciliation command.", 503)
	}
	return err
}

// ResumeProviders records local operator attestations and abandons retained
// provider work atomically. It never opens/migrates a Store or invokes providers.
func ResumeProviders(ctx context.Context, dataDir string, in ProviderResumptionRequest) (ProviderResumptionSummary, error) {
	var zero ProviderResumptionSummary
	if checkContext(ctx) != nil {
		return zero, providerResumptionValidationError(ctx, unavailable())
	}
	if !validResumptionRequest(in) || CheckDataDirectory(dataDir) != nil || privateDirectory(dataDir, false) != nil {
		return zero, invalid()
	}
	path := filepath.Join(dataDir, "firecontroller.sqlite")
	if info, err := privateFile(path); err != nil || info.Size() <= 0 {
		return zero, invalid()
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return zero, invalid()
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	u.RawQuery = url.Values{"mode": {"rw"}, "_pragma": {"foreign_keys(1)", "synchronous(FULL)", "busy_timeout(0)"}, "_txlock": {"immediate"}}.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return zero, unavailable()
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := beginProviderResumption(ctx, db)
	if err != nil {
		return zero, providerResumptionValidationError(ctx, unavailable())
	}
	defer tx.Rollback()
	if _, err = validateSchema(ctx, tx, true); err != nil {
		return zero, providerResumptionValidationError(ctx, err)
	}
	if err = activationIntegrity(ctx, tx); err != nil {
		return zero, providerResumptionValidationError(ctx, err)
	}
	status, err := readRecoveryStatus(ctx, tx)
	if err != nil {
		return zero, providerResumptionValidationError(ctx, err)
	}
	if status.State != "coordination_active" || status.InstanceID != in.ExpectedInstanceID || status.SourceInstanceID != in.FencedPredecessorID {
		return zero, activationConflict()
	}
	fields, err := readRestoreFields(ctx, tx)
	if err != nil {
		return zero, providerResumptionValidationError(ctx, err)
	}
	if !status.ProvidersHeld {
		release, err := readProviderRelease(fields, status)
		if err != nil {
			return zero, err
		}
		if in != release.Request {
			return zero, activationConflict()
		}
		return resumptionSummary(status, release), nil
	}
	at := time.Now().UTC()
	activation, _ := canonicalTime(status.ActivatedAt)
	if at.Before(activation) {
		at = activation
	}
	counts, err := reviewexec.AbandonRestoredProviderWork(ctx, tx, status.InstanceID, at)
	if err != nil {
		return zero, err
	}
	release := providerRelease{Version: 1, Request: in, BundleID: status.BundleID, ActivatedAt: status.ActivatedAt, ReleasedAt: at.Format(time.RFC3339Nano), Counts: counts}
	body, err := json.Marshal(release)
	if err != nil {
		return zero, unavailable()
	}
	fields["restore:provider_state"], fields["restore:provider_release"] = "released", string(body)
	archived, err := json.Marshal(fields)
	if err != nil || len(archived) > maxRecoveryHistoryBytes {
		return zero, core.Fail("restore_release_too_large", "The provider release audit exceeds its retained bound; shorten the reason or evidence reference.", 400)
	}
	if _, err = readProviderRelease(fields, status); err != nil {
		return zero, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE metadata SET value='released' WHERE key='restore:provider_state'`); err != nil {
		return zero, unavailable()
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES('restore:provider_release',?)`, string(body)); err != nil {
		return zero, unavailable()
	}
	if err = tx.Commit(); err != nil {
		return zero, unavailable()
	}
	return resumptionSummary(status, release), nil
}

// Retry lock acquisition only, before any reconciliation statement. SQLite's
// own busy wait cannot honor an early cancellation on this driver.
func beginProviderResumption(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	limit := time.NewTimer(5 * time.Second)
	defer limit.Stop()
	retry := time.NewTicker(10 * time.Millisecond)
	defer retry.Stop()
	for {
		if ctx.Err() != nil {
			return nil, unavailable()
		}
		tx, err := db.BeginTx(ctx, nil)
		if err == nil {
			return tx, nil
		}
		var coded interface{ Code() int }
		if !errors.As(err, &coded) || coded.Code()&255 != 5 && coded.Code()&255 != 6 {
			return nil, unavailable()
		}
		select {
		case <-ctx.Done():
			return nil, unavailable()
		case <-limit.C:
			return nil, unavailable()
		case <-retry.C:
		}
	}
}
