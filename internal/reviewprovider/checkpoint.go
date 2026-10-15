package reviewprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// CheckpointPayload binds original source facts. Mutable adjudication revisions
// and receiving proof do not participate in this historical publication.
type CheckpointPayload struct {
	PublicationID    string `json:"publication_id"`
	EvidenceID       string `json:"evidence_id"`
	RepositoryID     int64  `json:"repository_id"`
	PullNumber       int64  `json:"pull_number"`
	Channel          string `json:"channel"`
	HeadSHA          string `json:"head_sha"`
	BaseSHA          string `json:"base_sha"`
	BaseRef          string `json:"base_ref"`
	ParentSHA        string `json:"parent_sha"`
	MergeBaseSHA     string `json:"merge_base_sha"`
	PatchSHA256      string `json:"patch_sha256"`
	CaptureSHA256    string `json:"capture_sha256"`
	ParentPullNumber int64  `json:"parent_pull_number"`
	Found            int64  `json:"found"`
	Accepted         int64  `json:"accepted"`
	Routed           int64  `json:"routed"`
	Files            *int64 `json:"files,omitempty"`
	DurationSeconds  *int64 `json:"duration_seconds,omitempty"`
}

type CheckpointRender struct {
	Body           string
	SemanticSHA256 string
}

type CheckpointMatchState string

const (
	CheckpointUnrelated CheckpointMatchState = "unrelated"
	CheckpointMatch     CheckpointMatchState = "match"
	CheckpointConflict  CheckpointMatchState = "conflict"
)

func checkpointHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func validCheckpointPayload(p CheckpointPayload) bool {
	if !hostedTriggerID(p.PublicationID, "review_publication_") || !hostedTriggerID(p.EvidenceID, "review_evidence_") || p.RepositoryID <= 0 || !validPullNumber(p.PullNumber) || p.Channel != "cli" && p.Channel != "hosted" || !checkpointHex(p.HeadSHA, 40) || !checkpointHex(p.BaseSHA, 40) || !checkpointHex(p.ParentSHA, 40) || !checkpointHex(p.MergeBaseSHA, 40) || !checkpointHex(p.PatchSHA256, 64) || !checkpointHex(p.CaptureSHA256, 64) || len(p.BaseRef) > 256 || strings.TrimSpace(p.BaseRef) == "" || !utf8.ValidString(p.BaseRef) || p.ParentPullNumber < 0 || p.ParentPullNumber > 2147483647 || p.Found < 0 || p.Found > 100 || p.Accepted < 0 || p.Routed < 0 || p.Accepted > p.Found || p.Routed > p.Found-p.Accepted || p.Files != nil && *p.Files < 0 || p.DurationSeconds != nil && *p.DurationSeconds < 0 {
		return false
	}
	for _, r := range p.BaseRef {
		if unicode.IsControl(r) && r != '\t' && r != '\r' && r != '\n' {
			return false
		}
	}
	return true
}

// RenderCheckpoint hashes canonical JSON, including opaque publication/source
// identity and nil-versus-known optional facts. Only historical counts and the
// full captured head are visible; the remaining source facts stay behind SHA256.
func RenderCheckpoint(payload CheckpointPayload) (CheckpointRender, error) {
	if !validCheckpointPayload(payload) {
		return CheckpointRender{}, invalidSelector()
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return CheckpointRender{}, invalidSelector()
	}
	digest := sha256.Sum256(encoded)
	semantic := hex.EncodeToString(digest[:])
	label := "CLI"
	if payload.Channel == "hosted" {
		label = "Hosted"
	}
	line := fmt.Sprintf("%s: Found %d, Accepted %d, Routed %d; head %s", label, payload.Found, payload.Accepted, payload.Routed, payload.HeadSHA)
	if payload.Files != nil {
		line += fmt.Sprintf("; files %d", *payload.Files)
	}
	if payload.DurationSeconds != nil {
		line += fmt.Sprintf("; duration %d seconds", *payload.DurationSeconds)
	}
	body := line + "\nOriginal source counts; current review coverage and readiness are assessed separately.\n\n<!-- firecontroller-checkpoint:v1 publication=" + payload.PublicationID + " semantic=" + semantic + " -->"
	return CheckpointRender{Body: body, SemanticSHA256: semantic}, nil
}

// MatchCheckpoint accepts only neutral Unicode whitespace changes. Exact words,
// punctuation, counts, full head and marker tokens remain part of the match.
func MatchCheckpoint(body string, payload CheckpointPayload) (CheckpointMatchState, error) {
	render, err := RenderCheckpoint(payload)
	if err != nil {
		return CheckpointUnrelated, err
	}
	if !validBody(body) {
		return CheckpointUnrelated, invalidResponse(false, 0)
	}
	return matchCheckpoint(body, payload.PublicationID, render.Body), nil
}
func matchCheckpoint(body, publicationID, expected string) CheckpointMatchState {
	normalized := strings.Join(strings.Fields(body), " ")
	// Inspect actual comment markers, not an opaque ID in unrelated prose.
	// Tokenize delimiters once so nested/malformed copies cannot hide an owned
	// identity or turn bounded comment input into a quadratic suffix scan.
	marked := strings.ReplaceAll(strings.ReplaceAll(normalized, "<!--", " <!-- "), "-->", " --> ")
	owned, family, opening := false, false, false
	for _, token := range strings.Fields(marked) {
		switch token {
		case "<!--":
			opening, family = true, false
		case "-->":
			opening, family = false, false
		default:
			if opening {
				family = token == "firecontroller-checkpoint" || strings.HasPrefix(token, "firecontroller-checkpoint:")
				opening = false
			} else if family && strings.HasPrefix(token, "publication="+publicationID) {
				owned = true
			}
		}
	}
	if !owned {
		return CheckpointUnrelated
	}
	if normalized == strings.Join(strings.Fields(expected), " ") {
		return CheckpointMatch
	}
	return CheckpointConflict
}

// FindCheckpoint consumes a caller-proven complete inventory for exactly one
// repository/PR. No match is not proof that an uncertain post never happened.
// Stable author ID/kind owns attribution; cosmetic login changes are harmless.
func FindCheckpoint(payload CheckpointPayload, expectedActor Author, comments []IssueComment) (IssueComment, bool, error) {
	render, err := RenderCheckpoint(payload)
	if err != nil {
		return IssueComment{}, false, err
	}
	if _, err = normalizeActor(githubActor{ID: expectedActor.ID, Login: expectedActor.Login, Type: expectedActor.Kind}); err != nil {
		return IssueComment{}, false, invalidSelector()
	}
	if len(comments) > githubMaxItems {
		return IssueComment{}, false, invalidResponse(false, 0)
	}
	seen := make(map[int64]bool, len(comments))
	total := 0
	var result IssueComment
	found := false
	for _, comment := range comments {
		total += len(comment.Body)
		_, actorErr := normalizeActor(githubActor{ID: comment.Author.ID, Login: comment.Author.Login, Type: comment.Author.Kind})
		if total > githubMaxAggregate || comment.ID <= 0 || seen[comment.ID] || !validBody(comment.Body) || actorErr != nil || !hostedTriggerTime(comment.CreatedAt) || !hostedTriggerTime(comment.UpdatedAt) || comment.UpdatedAt.Before(comment.CreatedAt) {
			return IssueComment{}, false, invalidResponse(false, 0)
		}
		seen[comment.ID] = true
		if comment.Author.ID != expectedActor.ID || comment.Author.Kind != expectedActor.Kind {
			continue
		}
		switch matchCheckpoint(comment.Body, payload.PublicationID, render.Body) {
		case CheckpointConflict:
			return IssueComment{}, false, providerError("checkpoint_conflict", "The public checkpoint conflicts with its retained source identity.", 0, 0, false)
		case CheckpointMatch:
			if found {
				return IssueComment{}, false, providerError("checkpoint_ambiguous", "Multiple public checkpoints match the retained publication.", 0, 0, false)
			}
			result, found = comment, true
		}
	}
	return result, found, nil
}
