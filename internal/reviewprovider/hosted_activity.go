package reviewprovider

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HostedActivitySelector explicitly selects the operator's progress surface.
// A missing signal never implies that a different surface is available.
type HostedActivitySelector struct {
	Mode            string
	ProviderActorID int64
	AppID           int64
}

// HostedActivity describes observed activity only, never review coverage. SourceID
// identifies the selected status or a representative matching check; an idle
// check observation requires every matching check in the complete inventory to
// be completed. It does not select a newest check or correlate a review trigger.
type HostedActivity struct {
	State       string
	Reason      string
	ObservedAt  time.Time
	SourceID    int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
	StartedAt   time.Time
	CompletedAt time.Time
}

type githubActivityStatus struct {
	ID        int64       `json:"id,required"`
	Context   string      `json:"context,required"`
	State     string      `json:"state,required"`
	Creator   githubActor `json:"creator,required"`
	CreatedAt string      `json:"created_at,required"`
	UpdatedAt string      `json:"updated_at,required"`
}

type githubActivityCheck struct {
	ID      int64  `json:"id,required"`
	Name    string `json:"name,required"`
	HeadSHA string `json:"head_sha,required"`
	Status  string `json:"status,required"`
	App     struct {
		ID int64 `json:"id,required"`
	} `json:"app,required"`
	StartedAt   *string `json:"started_at"`
	CompletedAt *string `json:"completed_at"`
}

func activityTime(value string, observed time.Time) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, value)
	return t, err == nil && !t.IsZero() && value == t.UTC().Format(time.RFC3339Nano) && !t.After(observed)
}

// ObserveHostedActivity makes only the selected bounded GET collection. The
// complete collection shares one deadline; no redirects, retries, posts or
// fallback progress-surface discovery occur.
func (g *GitHub) ObserveHostedActivity(ctx context.Context, repository, head string, selector HostedActivitySelector) (HostedActivity, error) {
	result := HostedActivity{State: "unproven", Reason: "activity_not_proven"}
	if !validRepositorySelector(repository) || !validSHA(head) || strings.ToLower(head) != head || selector.ProviderActorID <= 0 ||
		selector.Mode != "commit_status" && selector.Mode != "check_run" || selector.Mode == "commit_status" && selector.AppID != 0 || selector.Mode == "check_run" && selector.AppID <= 0 {
		return result, invalidSelector()
	}
	if ctx == nil || ctx.Err() != nil || g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) {
		return result, providerError("provider_unavailable", "The provider activity observation is unavailable.", 0, 0, false)
	}
	ctx, cancel := context.WithTimeout(ctx, hostedObservationLimit)
	defer cancel()
	path := "/repos/" + repository + "/commits/" + head
	if selector.Mode == "commit_status" {
		var rows []githubActivityStatus
		if err := g.getPages(ctx, path+"/statuses", &rows); err != nil {
			return result, err
		}
		observed := time.Now().UTC()
		seen := make(map[int64]bool, len(rows))
		var selected *githubActivityStatus
		for i := range rows {
			r := &rows[i]
			created, ok := activityTime(r.CreatedAt, observed)
			updated, updatedOK := activityTime(r.UpdatedAt, observed)
			_, actorErr := normalizeActor(r.Creator)
			if r.ID <= 0 || !validPlainText(r.Context, 255) || strings.TrimSpace(r.Context) == "" || !validIdentifierText(r.State, 64) || actorErr != nil || !ok || !updatedOK || updated.Before(created) {
				return result, invalidResponse(false, http.StatusOK)
			}
			if seen[r.ID] {
				result.Reason, result.ObservedAt = "activity_ambiguous", observed
				return result, nil
			}
			seen[r.ID] = true
			if r.Creator.ID != selector.ProviderActorID || r.Creator.Type != "Bot" || !strings.EqualFold(r.Context, "CodeRabbit") {
				continue
			}
			if selected == nil || created.After(result.CreatedAt) || created.Equal(result.CreatedAt) && r.ID > selected.ID {
				selected = r
				result.SourceID, result.CreatedAt, result.UpdatedAt = r.ID, created, updated
			}
		}
		if ctx.Err() != nil {
			return HostedActivity{State: "unproven", Reason: "activity_not_proven"}, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
		}
		result.ObservedAt = observed
		if selected == nil {
			result.State, result.Reason = "idle", "no_provider_activity_observed"
			return result, nil
		}
		switch selected.State {
		case "pending":
			result.State, result.Reason = "active", "provider_activity_active"
		case "success", "failure", "error":
			result.State, result.Reason = "idle", "provider_activity_idle"
		default:
			result.Reason = "activity_state_unknown"
		}
		return result, nil
	}
	rows, err := g.hostedActivityChecks(ctx, path+"/check-runs")
	if err != nil {
		return result, err
	}
	observed := time.Now().UTC()
	seen := make(map[int64]githubActivityCheck, len(rows))
	matched, active, unknown := false, false, false
	for _, r := range rows {
		if r.ID <= 0 || r.App.ID <= 0 || !validPlainText(r.Name, 255) || strings.TrimSpace(r.Name) == "" || !validIdentifierText(r.Status, 64) || !validSHA(r.HeadSHA) || strings.ToLower(r.HeadSHA) != r.HeadSHA {
			return HostedActivity{State: "unproven", Reason: "activity_not_proven"}, invalidResponse(false, http.StatusOK)
		}
		var start, finish time.Time
		var ok bool
		if r.StartedAt != nil {
			if start, ok = activityTime(*r.StartedAt, observed); !ok {
				return HostedActivity{State: "unproven", Reason: "activity_not_proven"}, invalidResponse(false, http.StatusOK)
			}
		}
		if r.CompletedAt != nil {
			if finish, ok = activityTime(*r.CompletedAt, observed); !ok || !start.IsZero() && finish.Before(start) {
				return HostedActivity{State: "unproven", Reason: "activity_not_proven"}, invalidResponse(false, http.StatusOK)
			}
		}
		if prior, exists := seen[r.ID]; exists {
			if !sameActivityCheck(prior, r) {
				return HostedActivity{State: "unproven", Reason: "activity_ambiguous", ObservedAt: observed}, nil
			}
			continue
		}
		seen[r.ID] = r
		if r.App.ID != selector.AppID || r.HeadSHA != head || !strings.EqualFold(r.Name, "CodeRabbit") {
			continue
		}
		isActive := false
		switch r.Status {
		case "queued", "in_progress", "waiting", "requested", "pending":
			isActive = true
		case "completed":
			if finish.IsZero() {
				return HostedActivity{State: "unproven", Reason: "activity_not_proven"}, invalidResponse(false, http.StatusOK)
			}
		default:
			unknown = true
		}
		if !matched || isActive && !active {
			result.SourceID, result.StartedAt, result.CompletedAt = r.ID, start, finish
		}
		matched, active = true, active || isActive
	}
	if ctx.Err() != nil {
		return HostedActivity{State: "unproven", Reason: "activity_not_proven"}, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	result.ObservedAt = observed
	switch {
	case active:
		result.State, result.Reason = "active", "provider_activity_active"
	case unknown:
		result.Reason = "activity_state_unknown"
	case matched:
		result.State, result.Reason = "idle", "provider_activity_idle"
	default:
		result.State, result.Reason = "idle", "no_provider_activity_observed"
	}
	return result, nil
}

func sameActivityCheck(a, b githubActivityCheck) bool {
	optional := func(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
	return a.ID == b.ID && a.Name == b.Name && a.HeadSHA == b.HeadSHA && a.Status == b.Status && a.App == b.App && optional(a.StartedAt, b.StartedAt) && optional(a.CompletedAt, b.CompletedAt)
}

func (g *GitHub) hostedActivityChecks(ctx context.Context, path string) ([]githubActivityCheck, error) {
	var rows []githubActivityCheck
	aggregate := 0
	var total int64 = -1
	for page := int64(1); page <= githubMaxPages; page++ {
		fixed := url.Values{"filter": {"all"}}
		query := url.Values{"filter": {"all"}, "page": {strconv.FormatInt(page, 10)}, "per_page": {strconv.Itoa(githubPageSize)}}
		data, headers, status, err := g.requestBytes(ctx, http.MethodGet, path, query, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, statusError(&http.Response{StatusCode: status, Header: headers}, false)
		}
		aggregate += len(data)
		var envelope struct {
			TotalCount int64                 `json:"total_count,required"`
			CheckRuns  []githubActivityCheck `json:"check_runs,required"`
		}
		if aggregate > githubMaxAggregate || decodeProviderJSON(data, &envelope) != nil || envelope.CheckRuns == nil || len(envelope.CheckRuns) > githubPageSize || len(rows) > githubMaxItems-len(envelope.CheckRuns) || envelope.TotalCount < 0 || envelope.TotalCount > githubMaxItems || total >= 0 && envelope.TotalCount != total {
			return nil, invalidResponse(false, http.StatusOK)
		}
		total = envelope.TotalCount
		rows = append(rows, envelope.CheckRuns...)
		next, exists, err := nextPageWithQuery(headers.Values("Link"), g.baseURL, path, page, fixed)
		if err != nil || exists && (next != page+1 || page == githubMaxPages) {
			return nil, invalidResponse(false, http.StatusOK)
		}
		if !exists {
			// Exact duplicate rows can be reconciled, but must not stand in for
			// another advertised check that could still be active.
			ids := make(map[int64]bool, len(rows))
			for _, row := range rows {
				ids[row.ID] = true
			}
			if int64(len(ids)) != total {
				return nil, invalidResponse(false, http.StatusOK)
			}
			return rows, nil
		}
	}
	return nil, invalidResponse(false, http.StatusOK)
}
