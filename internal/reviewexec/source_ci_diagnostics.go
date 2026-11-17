package reviewexec

import "github.com/benhook1013/FireController/internal/reviewprovider"

// These are selected retained identities, never full provider inventories or
// newly collected results. Context state/reason remains the original assessment.
type SourceCIContext struct {
	Context      string                `json:"context"`
	AppID        int64                 `json:"app_id"`
	State        string                `json:"state"`
	Reason       string                `json:"reason"`
	CheckRun     *SourceCICheckRun     `json:"check_run,omitempty"`
	CommitStatus *SourceCICommitStatus `json:"commit_status,omitempty"`
}

type SourceCICheckRun struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type SourceCICommitStatus struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
}

func sourceCIContextDetails(o *reviewprovider.CIObservation) ([]SourceCIContext, error) {
	if o == nil {
		return nil, refreshStorageError()
	}
	checks := make([]SourceCIContext, 0, len(o.Assessment.Checks))
	runs := map[int64]reviewprovider.CICheckRun{}
	statuses := map[int64]reviewprovider.CICommitStatus{}
	if o.Results != nil {
		for _, c := range o.Results.Checks {
			runs[c.ID] = c
		}
		for _, s := range o.Results.Statuses {
			statuses[s.ID] = s
		}
	}
	for _, c := range o.Assessment.Checks {
		v := SourceCIContext{Context: c.Context, AppID: c.AppID, State: c.State, Reason: c.Reason}
		if c.CheckRunID != 0 {
			r, ok := runs[c.CheckRunID]
			if !ok || r.Name != c.Context {
				return nil, refreshStorageError()
			}
			v.CheckRun = &SourceCICheckRun{ID: r.ID, Status: r.Status, Conclusion: r.Conclusion}
		}
		if c.StatusID != 0 {
			s, ok := statuses[c.StatusID]
			if !ok || s.Context != c.Context {
				return nil, refreshStorageError()
			}
			v.CommitStatus = &SourceCICommitStatus{ID: s.ID, State: s.State}
		}
		checks = append(checks, v)
	}
	return checks, nil
}
