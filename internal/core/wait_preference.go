package core

import (
	"net/http"
	"strconv"
	"strings"
)

// WaitPreference selects only response waiting, never mutation identity.
// Canonical seconds and a single header avoid ambiguous proxy interpretations.
func WaitPreference(r *http.Request, defaultSeconds int) (int, error) {
	values, present := r.Header["Prefer"]
	if !present {
		return defaultSeconds, nil
	}
	invalid := Fail("validation_failed", "Use one Prefer: wait=SECONDS header from 0 through 900.", 400)
	if len(values) != 1 || !strings.HasPrefix(values[0], "wait=") {
		return 0, invalid
	}
	raw := strings.TrimPrefix(values[0], "wait=")
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 0 || seconds > 900 || strconv.Itoa(seconds) != raw {
		return 0, invalid
	}
	return seconds, nil
}
