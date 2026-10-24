package adoptionholdsmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var ErrInvalid = errors.New("invalid adoption obligation")

var hash = regexp.MustCompile(`^[a-f0-9]{64}$`)

func ValidText(value string, maximum int) bool {
	return utf8.ValidString(value) && len(value) > 0 && len(value) <= maximum && strings.TrimSpace(value) != "" && strings.IndexFunc(value, func(c rune) bool { return unicode.IsControl(c) && c != '\t' && c != '\n' && c != '\r' }) < 0
}

func ValidSourceKey(value string) bool {
	return ValidText(value, 200) && value == strings.TrimSpace(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

func ValidSource(source Source) bool {
	return ValidSourceKey(source.Namespace) && source.RepositoryID > 0 && hash.MatchString(source.SnapshotSHA256)
}

// ValidEvidence admits an explicitly empty, non-nil source evidence list.
// A source may state that its original evidence references are unknown.
func ValidEvidence(refs []string) bool {
	if refs == nil || len(refs) > 10 {
		return false
	}
	for _, ref := range refs {
		if !ValidText(ref, 1024) {
			return false
		}
	}
	return true
}

func ValidResolutionEvidence(refs []string) bool {
	return len(refs) >= 1 && ValidEvidence(refs)
}

func ValidPull(number int64) bool { return number >= 1 && number <= 2147483647 }

func ValidKind(kind string) bool {
	return kind == "finding" || kind == "route" || kind == "result"
}

func NormalizeObligation(obligation Obligation) (Obligation, error) {
	if !ValidSourceKey(obligation.SourceKey) || !ValidKind(obligation.Kind) || len(obligation.AffectedPullNumbers) < 1 || len(obligation.AffectedPullNumbers) > 100 || !ValidText(obligation.Summary, 16<<10) || !ValidEvidence(obligation.Evidence) || obligation.OriginalStatus != "pending" && obligation.OriginalStatus != "open" {
		return obligation, ErrInvalid
	}
	obligation.AffectedPullNumbers = slices.Clone(obligation.AffectedPullNumbers)
	slices.Sort(obligation.AffectedPullNumbers)
	for index, number := range obligation.AffectedPullNumbers {
		if !ValidPull(number) || index > 0 && number == obligation.AffectedPullNumbers[index-1] {
			return obligation, ErrInvalid
		}
	}
	obligation.Evidence = slices.Clone(obligation.Evidence)
	if len(Encode(obligation)) > 32<<10 {
		return obligation, ErrInvalid
	}
	return obligation, nil
}

// NormalizeBatch returns the canonical batch body used by preview binding and
// retained provenance. Its wrapper budget includes the apply fingerprint.
func NormalizeBatch(batch Batch) (Batch, []byte, error) {
	if batch.Version != 1 || !ValidSource(batch.Source) || batch.ExpectedRevision == nil || *batch.ExpectedRevision < 0 || *batch.ExpectedRevision == math.MaxInt64 || len(batch.Obligations) < 1 || len(batch.Obligations) > 100 {
		return batch, nil, ErrInvalid
	}
	revision := *batch.ExpectedRevision
	batch.ExpectedRevision = &revision
	batch.Obligations = slices.Clone(batch.Obligations)
	for index, obligation := range batch.Obligations {
		var err error
		batch.Obligations[index], err = NormalizeObligation(obligation)
		if err != nil {
			return batch, nil, err
		}
	}
	slices.SortFunc(batch.Obligations, func(left, right Obligation) int { return strings.Compare(left.SourceKey, right.SourceKey) })
	for index := 1; index < len(batch.Obligations); index++ {
		if batch.Obligations[index-1].SourceKey == batch.Obligations[index].SourceKey {
			return batch, nil, ErrInvalid
		}
	}
	body := Encode(batch)
	wrapper := Encode(ApplyRequest{Batch: batch, PreviewSHA256: strings.Repeat("0", 64)})
	if len(wrapper) > 256<<10 {
		return batch, nil, ErrInvalid
	}
	return batch, body, nil
}

func NormalizeResolution(resolution ResolveRequest) (ResolveRequest, error) {
	if resolution.ExpectedRevision == nil || *resolution.ExpectedRevision < 1 || *resolution.ExpectedRevision == math.MaxInt64 || !ValidText(resolution.Reason, 4096) || !ValidResolutionEvidence(resolution.Evidence) {
		return resolution, ErrInvalid
	}
	revision := *resolution.ExpectedRevision
	resolution.ExpectedRevision = &revision
	resolution.Evidence = slices.Clone(resolution.Evidence)
	if len(Encode(resolution)) > 16<<10 {
		return resolution, ErrInvalid
	}
	return resolution, nil
}

func Encode(value any) []byte { body, _ := json.Marshal(value); return body }

func Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
