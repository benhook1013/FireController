package adoptionholdsmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"slices"
	"strconv"
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

// An explicit zero leaves a route unassigned while retaining its source scope.
func NormalizeRetarget(request RetargetRequest) (RetargetRequest, error) {
	if request.ExpectedRevision == nil || *request.ExpectedRevision < 1 || *request.ExpectedRevision == math.MaxInt64 || request.TargetPullNumber == nil || *request.TargetPullNumber < 0 || *request.TargetPullNumber > 2147483647 || !ValidText(request.Reason, 4096) {
		return request, ErrInvalid
	}
	revision := *request.ExpectedRevision
	request.ExpectedRevision = &revision
	target := *request.TargetPullNumber
	request.TargetPullNumber = &target
	if len(Encode(request)) > 16<<10 {
		return request, ErrInvalid
	}
	return request, nil
}

// OriginalRouteTarget interprets the original explicit receiver marker only.
// Missing, conflicting or malformed markers remain unknown, represented by -1.
func OriginalRouteTarget(original Obligation) int64 {
	if original.Kind != "route" {
		return -1
	}
	target, count := int64(-1), 0
	for _, reference := range original.Evidence {
		if !strings.HasPrefix(reference, "target-pr:") {
			continue
		}
		count++
		value := strings.TrimPrefix(reference, "target-pr:")
		if value == "unassigned" {
			target = 0
			continue
		}
		number, err := strconv.ParseInt(value, 10, 32)
		if err != nil || !ValidPull(number) || strconv.FormatInt(number, 10) != value {
			target = -1
			continue
		}
		target = number
	}
	if count != 1 {
		return -1
	}
	return target
}

// RouteAffectedPullNumbers keeps original source scope and replaces only the
// original receiver whose identity is explicitly known from retained evidence.
func RouteAffectedPullNumbers(original Obligation, target int64) []int64 {
	out := make([]int64, 0, len(original.AffectedPullNumbers)+1)
	prior := OriginalRouteTarget(original)
	for _, number := range original.AffectedPullNumbers {
		if prior > 0 && number == prior {
			continue
		}
		out = append(out, number)
	}
	if target > 0 && !slices.Contains(out, target) {
		out = append(out, target)
	}
	slices.Sort(out)
	return out
}

func Encode(value any) []byte { body, _ := json.Marshal(value); return body }

func Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
