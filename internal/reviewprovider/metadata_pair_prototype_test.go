package reviewprovider

// Protocol proof. The provider seam is not wired into lifecycle scheduling.
import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func metadataPairFixture() map[string]any {
	repo := func() map[string]any {
		return map[string]any{"fullDatabaseId": "20", "nameWithOwner": "owner/repo", "defaultBranchRef": map[string]any{"name": "main"}}
	}
	pull := func(n int64) map[string]any {
		return map[string]any{"title": "Metadata proof", "number": n, "state": "OPEN", "merged": false, "headRefOid": candidateHead, "headRefName": "feature", "headRepository": repo(), "baseRefOid": candidateBase, "baseRefName": "main", "baseRepository": repo(), "changedFiles": 3, "additions": 4, "deletions": 2, "isDraft": false, "mergedAt": nil, "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN"}
	}
	return map[string]any{"data": map[string]any{"repository": map[string]any{"fullDatabaseId": "20", "nameWithOwner": "owner/repo", "p0": pull(7), "p1": pull(8)}, "rateLimit": map[string]any{"cost": 1}}}
}
func TestMetadataPairPrototypeExactNormalizedFields(t *testing.T) {
	for _, mode := range []string{"open", "draft", "unknown", "conflicting", "closed", "merged", "deleted_fork", "fork", "stale_base"} {
		t.Run(mode, func(t *testing.T) {
			f := metadataPairFixture()
			r := f["data"].(map[string]any)["repository"].(map[string]any)
			p := r["p0"].(map[string]any)
			var raw githubPull
			b, _ := json.Marshal(candidatePullFixture())
			if err := decodeProviderJSON(b, &raw); err != nil {
				t.Fatal(err)
			}
			raw.Title = "Metadata proof"
			raw.ChangedFiles = 3
			a, d := int64(4), int64(2)
			raw.Additions = &a
			raw.Deletions = &d
			draft := false
			raw.Draft = &draft
			headRef := "feature"
			raw.Head.Ref = &headRef
			raw.Head.Repo = &raw.Base.Repo
			raw.MergeState = "CLEAN"
			secondRaw := raw
			secondRaw.Number = 8
			secondDraft := false
			secondRaw.Draft = &secondDraft
			switch mode {
			case "draft":
				p["isDraft"] = true
				*raw.Draft = true
			case "unknown":
				p["mergeable"] = "UNKNOWN"
				raw.Mergeable = nil
			case "conflicting":
				p["mergeable"] = "CONFLICTING"
				v := false
				raw.Mergeable = &v
			case "closed":
				p["state"] = "CLOSED"
				raw.State = "closed"
			case "merged":
				p["state"] = "MERGED"
				p["merged"] = true
				stamp := "2026-11-17T13:04:29Z"
				p["mergedAt"] = stamp
				raw.State = "closed"
				raw.Merged = true
				raw.MergedAt = &stamp
			case "fork":
				p["headRepository"] = map[string]any{"fullDatabaseId": "45", "nameWithOwner": "fork/other", "defaultBranchRef": map[string]any{"name": "trunk"}}
				raw.Head.Repo = &githubRepository{ID: 45, FullName: "fork/other", DefaultBranch: "trunk"}
			case "deleted_fork":
				p["headRepository"] = nil
				raw.Head.Repo = nil
			case "stale_base":
				p["baseRefOid"] = strings.Repeat("e", 40)
				raw.Base.SHA = strings.Repeat("e", 40)
			}
			body, _ := json.Marshal(f)
			got := decodeMetadataPair(body, 20, "owner/repo", [2]int64{7, 8})
			want, err := normalizePull(raw, 7)
			wantRepo, repoErr := normalizeRepository(raw.Base.Repo, 20)
			wantSecond, secondErr := normalizePull(secondRaw, 8)
			if repoErr != nil || secondErr != nil || !reflect.DeepEqual(got[0].Repository, wantRepo) || !reflect.DeepEqual(got[1].Repository, wantRepo) || !reflect.DeepEqual(got[1].Pull, wantSecond) {
				t.Fatalf("repository or second member changed: %+v %v %v", got, repoErr, secondErr)
			}
			if err != nil || got[0].Err != nil || got[1].Err != nil || !reflect.DeepEqual(got[0].Pull, want) {
				t.Fatalf("same fields failed: %+v %+v %v", got[0], want, err)
			}
		})
	}
}
func TestMetadataPairPrototypeRefusesMalformedAndIsolatesMembers(t *testing.T) {
	for _, mode := range []string{"missing_field", "null_field", "wrong_number", "null_member", "wrong_repo", "overflow_id", "fractional_id", "missing_alias", "duplicate_alias", "partial_errors", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			f := metadataPairFixture()
			r := f["data"].(map[string]any)["repository"].(map[string]any)
			p := r["p0"].(map[string]any)
			all := false
			switch mode {
			case "missing_field":
				delete(p, "additions")
				all = true
			case "null_field":
				p["changedFiles"] = nil
				all = true
			case "wrong_number":
				p["number"] = 99
			case "null_member":
				r["p0"] = nil
			case "wrong_repo":
				p["baseRepository"].(map[string]any)["fullDatabaseId"] = "21"
			case "overflow_id":
				p["baseRepository"].(map[string]any)["fullDatabaseId"] = "9223372036854775808"
			case "fractional_id":
				p["baseRepository"].(map[string]any)["fullDatabaseId"] = "20.0"
			case "missing_alias":
				delete(r, "p0")
				all = true
			case "partial_errors":
				f["errors"] = []any{map[string]any{"type": "RATE_LIMITED"}}
				all = true
			case "duplicate_alias", "oversized":
				all = true
			}
			body, _ := json.Marshal(f)
			if mode == "duplicate_alias" {
				body = []byte(strings.Replace(string(body), `"p0":`, `"p0":null,"p0":`, 1))
			}
			if mode == "oversized" {
				body = append(body, []byte(strings.Repeat(" ", 64<<10))...)
			}
			got := decodeMetadataPair(body, 20, "owner/repo", [2]int64{7, 8})
			if got[0].Err == nil || (all && got[1].Err == nil) || (!all && got[1].Err != nil) {
				t.Fatal(mode, fmt.Sprint(got[0].Err), fmt.Sprint(got[1].Err))
			}
		})
	}
}
