package reviews

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"
)

func TestReviewWorkspaceControlsRequireProjectWrite(t *testing.T) {
	observation := fullImportRequest()
	pull := observation.Queue[0]
	pull.Complete = false
	transfer := transferRequest(0, TransferGroup{Source: "original-producer", Pulls: []PullInput{pull}})
	queue := queueEditRequest(0, []QueuePullInput{{Number: 7, Title: "Selected PR", State: "open", CIState: "unknown", Mergeability: "unknown"}}, nil)
	for _, test := range []struct {
		name, previewPath, applyPath string
		input                        any
		wrap                         func(string) any
	}{
		{name: "observation", applyPath: "/observations", input: observation},
		{name: "queue edit", applyPath: "/queue/edits", input: queue},
		{"import", "/import/preview", "/import/apply", observation, func(digest string) any { return ImportApplyRequest{digest, observation} }},
		{"transfer", "/transfer/preview", "/transfer/apply", transfer, func(digest string) any { return TransferApplyRequest{digest, transfer} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAPIFixture(t)
			const foreignProject = "foreign-review-project"
			const writer = "ordinary-review-writer"
			if _, err := f.store.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES(?, 'Other project',702,'owner/other')`, foreignProject); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte(writer))
			if _, err := f.store.DB().Exec(`INSERT INTO credentials(id,principal_id,token_sha256,is_admin,revoked) VALUES('review-writer','review-writer-principal',?,0,0)`, hex.EncodeToString(sum[:])); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.DB().Exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write,can_manage) VALUES('review-writer',?,1,1,0)`, f.project); err != nil {
				t.Fatal(err)
			}
			base, foreign := importBase(f), "/api/v1/projects/"+foreignProject+"/reviews"
			before := importDatabaseState(t, f)
			body := importJSON(t, test.input)
			if test.previewPath != "" {
				requireImportResponse(t, importHTTP(f, base+test.previewPath, f.readToken, "", "", body), 403, "forbidden")
				requireImportResponse(t, importHTTP(f, foreign+test.previewPath, writer, "", "", body), 403, "forbidden")
				preview := importHTTP(f, base+test.previewPath, writer, "", "", body)
				requireImportResponse(t, preview, 200, "")
				var binding struct {
					Digest string `json:"preview_sha256"`
				}
				if err := json.Unmarshal(preview.Body.Bytes(), &binding); err != nil || len(binding.Digest) != 64 {
					t.Fatalf("invalid preview binding: %v", err)
				}
				body = importJSON(t, test.wrap(binding.Digest))
			}
			const key = "writer-control"
			requireImportResponse(t, importHTTP(f, base+test.applyPath, f.readToken, "", key, body), 403, "forbidden")
			requireImportResponse(t, importHTTP(f, foreign+test.applyPath, writer, "", key, body), 403, "forbidden")
			if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
				t.Fatal("preview or unauthorized request changed durable state")
			}
			first := importHTTP(f, base+test.applyPath, writer, "", key, body)
			requireImportResponse(t, first, 201, "")
			replay := importHTTP(f, base+test.applyPath, writer, "", key, body)
			requireImportResponse(t, replay, 201, "")
			if replay.Body.String() != first.Body.String() {
				t.Fatal("original receipt changed on replay")
			}
			if _, err := f.store.DB().Exec(`UPDATE grants SET can_write=0 WHERE credential_id='review-writer'`); err != nil {
				t.Fatal(err)
			}
			requireImportResponse(t, importHTTP(f, base+test.applyPath, writer, "", key, body), 403, "forbidden")
		})
	}
}
