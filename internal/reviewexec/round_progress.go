package reviewexec

import (
	"context"
	"database/sql"

	"github.com/benhook1013/FireController/internal/core"
)

// RoundProgressChannel contains current native allowance facts only. Pending
// counts operations bound to this epoch, not coverage or permission to dispatch.
type RoundProgressChannel struct {
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
	Pending          int64  `json:"pending"`
}

type RoundProgressItem struct {
	PullNumber int64                  `json:"pull_number"`
	Channels   []RoundProgressChannel `json:"channels"`
}

type RoundProgress struct {
	ProjectID   string              `json:"project_id"`
	EvaluatedAt string              `json:"evaluated_at"`
	Items       []RoundProgressItem `json:"items"`
}

func validRoundProgressNumbers(numbers []int64) bool {
	if len(numbers) < 1 || len(numbers) > 100 {
		return false
	}
	seen := make(map[int64]bool, len(numbers))
	for _, number := range numbers {
		if number < 1 || number > 2147483647 || seen[number] {
			return false
		}
		seen[number] = true
	}
	return true
}

// ReadRoundProgress reads one bounded, authorized local snapshot. It does not
// interpret imported history or collect source, captures, CI or provider state.
func (e *Engine) ReadRoundProgress(ctx context.Context, scope core.Scope, numbers []int64) (RoundProgress, error) {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(scope.ProjectID) || !validRoundProgressNumbers(numbers) {
		return RoundProgress{}, invalid()
	}
	numbers = append([]int64(nil), numbers...)
	out := RoundProgress{ProjectID: scope.ProjectID, Items: make([]RoundProgressItem, 0, len(numbers))}
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		for _, number := range numbers {
			item := RoundProgressItem{PullNumber: number, Channels: make([]RoundProgressChannel, 0, 2)}
			for _, channel := range []string{"hosted", "cli"} {
				control, err := readRoundRecord(ctx, tx, scope.ProjectID, number, channel)
				if err != nil {
					return err
				}
				var pending int64
				if control.Configured {
					pending, err = roundOccupied(ctx, tx, control, "")
					if err != nil {
						return err
					}
				}
				item.Channels = append(item.Channels, RoundProgressChannel{
					Channel: channel, Configured: control.Configured, Revision: control.Revision,
					Epoch: control.Epoch, Closed: control.Closed, Minimum: control.Minimum,
					Maximum: control.Maximum, MaximumUnbounded: control.MaximumUnbounded, Taper: control.Taper, Used: control.Used,
					Dry: control.Dry, Tapered: control.Tapered, Pending: pending,
				})
			}
			out.Items = append(out.Items, item)
		}
		out.EvaluatedAt = stamp()
		return nil
	})
	if err != nil {
		return RoundProgress{}, err
	}
	return out, nil
}
