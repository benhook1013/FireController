package reviews

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

func normalizeTransfer(req *TransferRequest) ([]byte, error) {
	if req.Version != 1 || req.ExpectedRevision == nil || *req.ExpectedRevision < 0 || req.RepositoryID <= 0 || !validText(req.Publisher, 128, true) {
		return nil, validationError("Provide transfer version one, an expected revision, repository identity and bounded publisher label.")
	}
	if req.Groups == nil {
		req.Groups = []TransferGroup{}
	}
	if len(req.Groups) < 1 || len(req.Groups) > 100 {
		return nil, validationError("Select between one and one hundred transfer producer groups.")
	}
	sources := make(map[string]bool, len(req.Groups))
	pulls := make(map[int64]PullInput)
	var appearances, runs, attempts, findings int
	for index := range req.Groups {
		group := &req.Groups[index]
		if !validText(group.Source, 128, true) || sources[group.Source] {
			return nil, validationError("Transfer groups require unique bounded original producer labels.")
		}
		sources[group.Source] = true
		if group.Pulls == nil {
			group.Pulls = []PullInput{}
		}
		if len(group.Pulls) == 0 {
			return nil, validationError("Transfer groups require at least one selected pull.")
		}
		if err := validateObservationContent(group.Pulls, nil, false); err != nil {
			return nil, err
		}
		appearances += len(group.Pulls)
		if appearances > maxQueuePulls {
			return nil, validationError("The transfer exceeds its bounded pull appearance count.")
		}
		for _, pull := range group.Pulls {
			if pull.Complete {
				return nil, validationError("Transferred pull metadata must declare incomplete coverage.")
			}
			if previous, exists := pulls[pull.Number]; exists && !sameTransferPullMetadata(previous, pull) {
				return nil, validationError("Repeated pull metadata must agree across original producers.")
			}
			pulls[pull.Number] = pull
			runs += len(pull.Runs)
			attempts += len(pull.Attempts)
			for _, run := range pull.Runs {
				findings += len(run.Findings)
			}
			if runs > maxSnapshotRuns || attempts > maxSnapshotAttempts || findings > maxSnapshotFindings {
				return nil, validationError("The transfer exceeds its bounded run, attempt or finding count.")
			}
		}
	}
	body, err := json.Marshal(req)
	if err != nil || len(body) > maxObservationBytes {
		return nil, validationError("The normalized transfer exceeds 256 KiB.")
	}
	wrapped, err := json.Marshal(TransferApplyRequest{PreviewSHA256: strings.Repeat("0", 64), Batch: *req})
	if err != nil || len(wrapped) > maxObservationBytes {
		return nil, validationError("The normalized transfer apply request exceeds 256 KiB.")
	}
	return body, nil
}

func sameTransferPullMetadata(a, b PullInput) bool {
	return a.Number == b.Number && a.Title == b.Title && a.State == b.State && a.HeadSHA == b.HeadSHA && a.BaseRef == b.BaseRef && a.BaseSHA == b.BaseSHA && a.CIState == b.CIState && a.Mergeability == b.Mergeability && a.Complete == b.Complete && mustJSON(pullMetadata(a)) == mustJSON(pullMetadata(b))
}

func transferPreview(instanceID string, scope core.Scope, req TransferRequest, body []byte) (TransferPreview, error) {
	binding, err := json.Marshal(struct {
		Domain      string          `json:"domain"`
		Version     int             `json:"version"`
		InstanceID  string          `json:"instance_id"`
		ProjectID   string          `json:"project_id"`
		PrincipalID string          `json:"principal_id"`
		WorkerID    string          `json:"worker_id"`
		Batch       json.RawMessage `json:"batch"`
	}{"firecontroller.review-transfer.preview", 1, instanceID, scope.ProjectID, scope.PrincipalID, scope.WorkerID, body})
	if err != nil {
		return TransferPreview{}, validationError("The transfer preview could not be encoded.")
	}
	payloadHash, previewHash := sha256.Sum256(body), sha256.Sum256(binding)
	response := TransferPreview{Version: 1, ProjectID: scope.ProjectID, RepositoryID: req.RepositoryID, Publisher: req.Publisher, ExpectedRevision: *req.ExpectedRevision, PayloadSHA256: hex.EncodeToString(payloadHash[:]), PreviewSHA256: hex.EncodeToString(previewHash[:]), ImportedHistory: true, NativeCoverage: false, Coverage: "partial"}
	pulls := make(map[int64]bool)
	for _, group := range req.Groups {
		for _, pull := range group.Pulls {
			pulls[pull.Number] = true
			response.Counts.Runs += int64(len(pull.Runs))
			response.Counts.Attempts += int64(len(pull.Attempts))
			for _, run := range pull.Runs {
				response.Counts.Findings += int64(len(run.Findings))
			}
		}
	}
	response.Counts.Pulls = int64(len(pulls))
	return response, nil
}
