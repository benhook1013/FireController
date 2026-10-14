package core

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/strictjson"
)

func validIdempotencyKey(value string) bool {
	return value != "" && len(value) <= 128 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n\x00") && utf8.ValidString(value)
}

// IdempotencyKey admits one bounded key without normalizing its identity.
// Callers select the error code owned by their route contract.
func IdempotencyKey(r *http.Request, code string) (string, error) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 || !validIdempotencyKey(values[0]) {
		return "", Fail(code, "Use one valid nonempty Idempotency-Key of at most 128 bytes without surrounding whitespace.", http.StatusBadRequest)
	}
	return values[0], nil
}

// QueryKeys rejects malformed encoding and unsupported or repeated fields.
// Value validation and route-specific size bounds remain with the caller.
func QueryKeys(r *http.Request, code string, allowed ...string) (url.Values, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, Fail(code, "Use valid URL query encoding.", http.StatusBadRequest)
	}
	for name, values := range query {
		known := false
		for _, candidate := range allowed {
			if name == candidate {
				known = true
				break
			}
		}
		if !known || len(values) != 1 {
			return nil, Fail(code, "Use supported query fields once each.", http.StatusBadRequest)
		}
	}
	return query, nil
}

func ReadJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return Fail("validation_failed", "The request body is unavailable or exceeds 256 KiB.", 400)
	}
	if strictjson.Decode(data, dst) != nil {
		return Fail("validation_failed", "Use one strict JSON object with declared fields, unique keys and valid Unicode.", 400)
	}
	return nil
}
