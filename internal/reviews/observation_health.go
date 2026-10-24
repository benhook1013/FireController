package reviews

import "context"

// ObserverHealth describes the running service, independently of immutable
// snapshot timestamps. It establishes neither coverage nor merge readiness.
type ObserverHealth struct {
	State     string `json:"state"`
	StartedAt string `json:"started_at,omitempty"`
}
type PullObservationHealth struct {
	State           string `json:"state"`
	CheckedAt       string `json:"checked_at,omitempty"`
	LastSuccessAt   string `json:"last_success_at,omitempty"`
	NextCheckAt     string `json:"next_check_at,omitempty"`
	Error           string `json:"error,omitempty"`
	CICheckedAt     string `json:"ci_checked_at,omitempty"`
	CIError         string `json:"ci_error,omitempty"`
	HostedCheckedAt string `json:"hosted_checked_at,omitempty"`
	HostedError     string `json:"hosted_error,omitempty"`
}
type ObservationHealthReader func(context.Context, string, []PullSummary) (ObserverHealth, map[int64]PullObservationHealth, error)

func attachObservationHealth(ctx context.Context, project string, response *QueueResponse, readers []ObservationHealthReader) error {
	if len(readers) == 0 || readers[0] == nil {
		return nil
	}
	observer, pulls, err := readers[0](ctx, project, response.Items)
	if err != nil {
		return err
	}
	response.Observer = &observer
	for i := range response.Items {
		if health, ok := pulls[response.Items[i].Number]; ok {
			response.Items[i].Observation = &health
		}
	}
	return nil
}
