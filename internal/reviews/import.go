package reviews

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

// ImportCounts describes retained history, not native review coverage.
type ImportCounts struct {
	Pulls       int64 `json:"pulls"`
	Runs        int64 `json:"runs"`
	Attempts    int64 `json:"attempts"`
	Findings    int64 `json:"findings"`
	Retirements int64 `json:"retirements"`
}

// ImportPreview is a compact, stateless content and destination binding.
// It grants neither authorization nor a reservation against later changes.
type ImportPreview struct {
	Version          int          `json:"version"`
	ProjectID        string       `json:"project_id"`
	RepositoryID     int64        `json:"repository_id"`
	Source           string       `json:"source"`
	ObservedAt       string       `json:"observed_at"`
	ExpectedRevision int64        `json:"expected_revision"`
	PayloadSHA256    string       `json:"payload_sha256"`
	PreviewSHA256    string       `json:"preview_sha256"`
	Counts           ImportCounts `json:"counts"`
	ImportedHistory  bool         `json:"imported_history"`
	NativeCoverage   bool         `json:"native_coverage"`
}

type ImportApplyRequest struct {
	PreviewSHA256 string             `json:"preview_sha256"`
	Observation   ObservationRequest `json:"observation"`
}

func previewImport(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.AdminScope(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
			core.WriteError(w, err)
			return
		}
		var req ObservationRequest
		if err := core.ReadJSON(w, r, &req); err != nil {
			core.WriteError(w, err)
			return
		}
		body, err := normalizeImport(&req)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		var response ImportPreview
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := admitObservation(ctx, tx, scope.ProjectID, req); err != nil {
				return err
			}
			var err error
			response, err = importPreview(store.InstanceID(), scope, req, body)
			return err
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusOK, response)
	}
}

func applyImport(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := store.AdminScope(r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if _, err := core.QueryKeys(r, "validation_failed"); err != nil {
			core.WriteError(w, err)
			return
		}
		var req ImportApplyRequest
		if err := core.ReadJSON(w, r, &req); err != nil {
			core.WriteError(w, err)
			return
		}
		if len(req.PreviewSHA256) != 64 || strings.Trim(req.PreviewSHA256, "0123456789abcdef") != "" {
			core.WriteError(w, validationError("preview_sha256 must be 64 lowercase hexadecimal characters."))
			return
		}
		body, err := normalizeImport(&req.Observation)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		preview, err := importPreview(store.InstanceID(), scope, req.Observation, body)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if req.PreviewSHA256 != preview.PreviewSHA256 {
			core.WriteError(w, core.Fail("import_preview_mismatch", "The observation or destination differs from the preview; preview the selected document again.", http.StatusConflict))
			return
		}
		key, err := core.IdempotencyKey(r, "validation_failed")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		// Use the original observation request as the receipt identity. The
		// fingerprint is not a second mutation, and replay precedes admission.
		result, err := recordObservation(r.Context(), store, scope, key, req.Observation, body)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, http.StatusCreated, json.RawMessage(result))
	}
}

func normalizeImport(req *ObservationRequest) ([]byte, error) {
	body, err := normalizeObservation(req)
	if err != nil {
		return nil, err
	}
	// The ordinary observation limit also bounds the entire apply wrapper.
	// Check normalized encoding: legal source characters may expand in JSON.
	wrapped, err := json.Marshal(ImportApplyRequest{PreviewSHA256: strings.Repeat("0", 64), Observation: *req})
	if err != nil || len(wrapped) > maxObservationBytes {
		return nil, validationError("The normalized import apply request exceeds 256 KiB.")
	}
	return body, nil
}

func importPreview(instanceID string, scope core.Scope, req ObservationRequest, body []byte) (ImportPreview, error) {
	binding, err := json.Marshal(struct {
		Domain      string          `json:"domain"`
		Version     int             `json:"version"`
		InstanceID  string          `json:"instance_id"`
		ProjectID   string          `json:"project_id"`
		PrincipalID string          `json:"principal_id"`
		WorkerID    string          `json:"worker_id"`
		Observation json.RawMessage `json:"observation"`
	}{"firecontroller.review-import.preview", 1, instanceID, scope.ProjectID, scope.PrincipalID, scope.WorkerID, body})
	if err != nil {
		return ImportPreview{}, validationError("The import preview could not be encoded.")
	}
	payloadHash, previewHash := sha256.Sum256(body), sha256.Sum256(binding)
	response := ImportPreview{Version: 1, ProjectID: scope.ProjectID, RepositoryID: req.RepositoryID,
		Source: req.Source, ObservedAt: req.ObservedAt, ExpectedRevision: *req.ExpectedRevision,
		PayloadSHA256: hex.EncodeToString(payloadHash[:]), PreviewSHA256: hex.EncodeToString(previewHash[:]),
		Counts:          ImportCounts{Pulls: int64(len(req.Queue)), Retirements: int64(len(req.Retired))},
		ImportedHistory: true, NativeCoverage: false}
	for _, pull := range req.Queue {
		response.Counts.Runs += int64(len(pull.Runs))
		response.Counts.Attempts += int64(len(pull.Attempts))
		for _, run := range pull.Runs {
			response.Counts.Findings += int64(len(run.Findings))
		}
	}
	return response, nil
}
