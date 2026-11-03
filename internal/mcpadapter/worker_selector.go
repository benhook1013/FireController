package mcpadapter

import (
	"bytes"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// selectedWorkerArguments removes only the explicit resource selector. It never
// changes the profile's acting worker or authorizes another project's resource.
func selectedWorkerArguments(raw json.RawMessage) (json.RawMessage, string, bool) {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if len(raw) > 16<<10 || strictjson.Validate(raw) != nil {
		return nil, "", false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, "", false
	}
	var worker string
	if value, exists := fields["worker_id"]; exists {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &worker) != nil || !validID(worker) {
			return nil, "", false
		}
		delete(fields, "worker_id")
	}
	cleaned, err := json.Marshal(fields)
	return cleaned, worker, err == nil
}
