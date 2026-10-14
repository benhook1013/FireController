package core

import (
	"io"
	"net/http"

	"github.com/benhook1013/FireController/internal/strictjson"
)

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
