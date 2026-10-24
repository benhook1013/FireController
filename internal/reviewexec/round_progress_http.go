package reviewexec

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

func roundProgressNumbers(r *http.Request) ([]int64, error) {
	if len(r.URL.RawQuery) > 4096 {
		return nil, invalid()
	}
	query, err := core.QueryKeys(r, "validation_failed", "pull_numbers", "view")
	if err != nil {
		return nil, err
	}
	if query.Has("view") && query.Get("view") != "stages" {
		return nil, invalid()
	}
	raw := query.Get("pull_numbers")
	// One hundred int32 values and their separating commas need at most 1099 bytes.
	if raw == "" || len(raw) > 1099 {
		return nil, invalid()
	}
	values := strings.Split(raw, ",")
	if len(values) > 100 {
		return nil, invalid()
	}
	numbers := make([]int64, 0, len(values))
	for _, value := range values {
		number, err := strconv.ParseInt(value, 10, 32)
		if err != nil || strconv.FormatInt(number, 10) != value {
			return nil, invalid()
		}
		numbers = append(numbers, number)
	}
	if !validRoundProgressNumbers(numbers) {
		return nil, invalid()
	}
	return numbers, nil
}

func (e *Engine) readRoundProgressHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.store.Scope(r, "read")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	numbers, err := roundProgressNumbers(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	progress, err := e.readRoundProgress(r.Context(), scope, numbers, r.URL.Query().Get("view") == "stages")
	if err != nil {
		core.WriteError(w, err)
		return
	}
	core.WriteJSON(w, http.StatusOK, progress)
}
