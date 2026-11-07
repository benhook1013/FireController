package adoptionholds

import (
	"encoding/json"
	m "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/core"
	"regexp"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var hash = regexp.MustCompile(`^[a-f0-9]{64}$`)

func invalid() error {
	return core.Fail("validation_failed", "Supply a bounded explicit carried-obligation request.", 400)
}
func unavailable() error {
	return core.Fail("storage_unavailable", "Retained adoption obligations are unavailable.", 500)
}
func conflict() error {
	return core.Fail("adoption_obligations_conflict", "The source revision, repository or retained original facts conflict.", 409)
}
func text(value string, maximum int) bool {
	return m.ValidText(value, maximum)
}
func sourceKey(value string) bool {
	return m.ValidSourceKey(value)
}
func validSource(s m.Source) bool {
	return m.ValidSource(s)
}
func evidence(refs []string) bool {
	return m.ValidEvidence(refs)
}
func pull(n int64) bool { return m.ValidPull(n) }
func NormalizeObligation(o m.Obligation) (m.Obligation, error) {
	normalized, err := m.NormalizeObligation(o)
	if err != nil {
		return o, invalid()
	}
	return normalized, nil
}
func NormalizeBatch(b m.Batch) (m.Batch, []byte, error) {
	normalized, body, err := m.NormalizeBatch(b)
	if err != nil {
		return b, nil, invalid()
	}
	return normalized, body, nil
}
func normalizeResolution(r m.ResolveRequest) (m.ResolveRequest, error) {
	normalized, err := m.NormalizeResolution(r)
	if err != nil {
		return r, invalid()
	}
	return normalized, nil
}
func encode(value any) []byte   { body, _ := json.Marshal(value); return body }
func digest(body []byte) string { return m.Digest(body) }
