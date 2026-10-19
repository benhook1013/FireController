package reviewprovider

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// ValidateHistoryFacts validates one retained page of normalized adapter facts,
// not GitHub wire JSON. It returns identities in retained source order. It does
// not establish cross-page completeness, import authority or native coverage.
func ValidateHistoryFacts(resource string, facts []byte) ([]string, error) {
	if len(facts) > githubMaxBody || strictjson.Validate(facts) != nil {
		return nil, invalidResponse(false, http.StatusOK)
	}
	if resource == "pull" {
		var pull Pull
		if !historyFactShape(facts, reflect.TypeOf(pull)) || json.Unmarshal(facts, &pull) != nil || !validHistoryPull(pull) {
			return nil, invalidResponse(false, http.StatusOK)
		}
		return []string{strconv.FormatInt(pull.Number, 10)}, nil
	}
	var rows []json.RawMessage
	if json.Unmarshal(facts, &rows) != nil || rows == nil || len(rows) > githubPageSize {
		return nil, invalidResponse(false, http.StatusOK)
	}
	ids := make([]string, 0, len(rows))
	seen := make(map[int64]bool, len(rows))
	for _, row := range rows {
		var id int64
		var valid bool
		switch resource {
		case "reviews":
			var v HostedReview
			if historyFactShape(row, reflect.TypeOf(v)) && json.Unmarshal(row, &v) == nil {
				var at *string
				if v.State != "pending" || !v.SubmittedAt.IsZero() {
					s := v.SubmittedAt.Format(time.RFC3339Nano)
					at = &s
				}
				normalized, err := normalizeHostedReview(githubHostedReview{ID: v.ID, Body: v.Body, State: v.State, CommitID: v.HeadSHA, User: historyActor(v.Author), SubmittedAt: at})
				id, valid = v.ID, err == nil && normalized.State == v.State
			}
		case "review_comments":
			var v ReviewComment
			if historyFactShape(row, reflect.TypeOf(v)) && json.Unmarshal(row, &v) == nil {
				var parent *int64
				if v.InReplyToID != 0 {
					parent = &v.InReplyToID
				}
				_, err := normalizeReviewComment(githubReviewComment{ID: v.ID, ReviewID: v.ReviewID, InReplyToID: parent, Body: v.Body, User: historyActor(v.Author), Path: v.Path, Line: v.Line, StartLine: v.StartLine, OriginalLine: v.OriginalLine, OriginalStartLine: v.OriginalStartLine, Side: historySide(v.Side), StartSide: historySide(v.StartSide), CommitID: v.HeadSHA, OriginalCommit: v.OriginalHeadSHA, CreatedAt: v.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: v.UpdatedAt.Format(time.RFC3339Nano)})
				id, valid = v.ID, err == nil
			}
		case "issue_comments":
			var v IssueComment
			if historyFactShape(row, reflect.TypeOf(v)) && json.Unmarshal(row, &v) == nil {
				_, err := normalizeIssueComment(githubIssueComment{ID: v.ID, Body: v.Body, User: historyActor(v.Author), CreatedAt: v.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: v.UpdatedAt.Format(time.RFC3339Nano)})
				id, valid = v.ID, err == nil
			}
		default:
			return nil, invalidResponse(false, http.StatusOK)
		}
		if !valid || seen[id] {
			return nil, invalidResponse(false, http.StatusOK)
		}
		seen[id] = true
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	// Even an empty page must name a recognized collection resource.
	if resource != "reviews" && resource != "review_comments" && resource != "issue_comments" {
		return nil, invalidResponse(false, http.StatusOK)
	}
	return ids, nil
}

func historyActor(a Author) githubActor { return githubActor{ID: a.ID, Login: a.Login, Type: a.Kind} }
func historySide(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func validHistoryPull(p Pull) bool {
	// The normalized Pull retains only repository identity, not the provider's
	// repository-name/default-branch object; validate only its retained fields.
	return validPullNumber(p.Number) && p.RepositoryID > 0 && validPlainText(p.Title, 4096) && strings.TrimSpace(p.Title) != "" && (p.State == "open" || p.State == "closed") && (!p.Merged || p.State == "closed") && validSHA(p.HeadSHA) && validSHA(p.BaseSHA) && validPlainText(p.BaseRef, 255) && strings.TrimSpace(p.BaseRef) != "" && p.ChangedFiles >= 0 && p.ChangedFiles <= 1000000 && validPullDisplayMetadata(p)
}

// Unlike provider wire decoding, retained facts have an exact, fully emitted
// field set except explicitly optional Pull display facts added after legacy
// captures. Missing facts stay unknown; aliases and unknown fields are refused.
func historyFactShape(data []byte, t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			return true
		}
		return historyFactShape(data, t.Elem())
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return false
	}
	if t == reflect.TypeOf(time.Time{}) {
		var s string
		if json.Unmarshal(data, &s) != nil {
			return false
		}
		_, err := parseTimestamp(s)
		return err == nil
	}
	if t.Kind() != reflect.Struct {
		return true
	} // Typed unmarshal checks scalar types and integer bounds.
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil || len(fields) > t.NumField() {
		return false
	}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		raw, ok := fields[field.Name]
		if !ok && t == reflect.TypeOf(Pull{}) && strings.Contains(field.Tag.Get("json"), "omitempty") {
			continue
		}
		if !ok || !historyFactShape(raw, field.Type) {
			return false
		}
	}
	for name := range fields {
		if _, ok := t.FieldByName(name); !ok {
			return false
		}
	}
	return true
}

func validPullDisplayMetadata(p Pull) bool {
	if (p.Additions == nil) != (p.Deletions == nil) {
		return false
	}
	for _, n := range []*int64{p.Additions, p.Deletions} {
		if n != nil && (*n < 0 || *n > 2147483647) {
			return false
		}
	}
	if p.MergedAt != "" {
		if !p.Merged || len(p.MergedAt) > 64 {
			return false
		}
		if _, err := time.Parse(time.RFC3339Nano, p.MergedAt); err != nil {
			return false
		}
	}
	return true
}
