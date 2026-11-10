package inbox_test

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/server"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerRetirementRealParentEmptyACKAndRetiredSendReplay(t *testing.T) {
	root := os.Getenv("FC_ADMIN_PARENT_FIXTURE")
	if root == "" {
		t.Skip("requires actual parent-produced receipts")
	}
	body, err := os.ReadFile(filepath.Join(root, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]string
	if json.Unmarshal(body, &f) != nil {
		t.Fatal("fixture metadata")
	}
	dir := filepath.Join(t.TempDir(), "copy")
	err = filepath.WalkDir(filepath.Join(root, "source"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(filepath.Join(root, "source"), path)
		if err != nil {
			return err
		}
		dest := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, b, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mux := server.New(s)
	inbox.Register(mux, s)
	call := func(path, key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+f["token"])
		r.Header.Set("X-FC-Worker", f["worker"])
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	base := "/api/v1/projects/" + f["project"]
	var before int
	if err := s.DB().QueryRow(`SELECT count(*) FROM receipts`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	w := call(base+"/messages/"+f["message"]+"/ack", "parent-empty-ack", "{}")
	if w.Code != 200 || w.Body.String() != f["ack_response"] {
		t.Fatal("legacy ACK changed", w.Code, w.Body.String())
	}
	explicit, _ := json.Marshal(map[string]string{"worker_id": f["worker"]})
	w = call(base+"/messages/"+f["message"]+"/ack", "parent-empty-ack", string(explicit))
	if w.Code != 409 {
		t.Fatal("different body reused parent receipt", w.Code)
	}
	var after int
	s.DB().QueryRow(`SELECT count(*) FROM receipts`).Scan(&after)
	if before != after {
		t.Fatal("replay mutated receipts")
	}
	var sendKey, original string
	if err := s.DB().QueryRow(`SELECT key,response_json FROM receipts WHERE project_id=? AND operation='messages.send'`, f["project"]).Scan(&sendKey, &original); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO worker_retirements VALUES(?,?, '2026-11-01T00:00:00Z','synthetic-actor')`, f["project"], f["worker"]); err != nil {
		t.Fatal(err)
	}
	message, _ := json.Marshal(map[string]string{"recipient_worker_id": f["worker"], "body": "Retained message with literal <text>"})
	w = call(base+"/messages", sendKey, string(message))
	if w.Code != 201 || strings.TrimSpace(w.Body.String()) != original {
		t.Fatal("retired sender lost exact history", w.Code, w.Body.String(), original)
	}
	w = call(base+"/messages", "new-after-retirement", string(message))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "worker_retired") {
		t.Fatal("new retired send admitted", w.Code, w.Body.String())
	}
}
