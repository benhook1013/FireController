package mcpadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestReviewRoundsMCPArgumentsBeforeCredentials(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	binding := localContext()
	binding.ServerURL = server.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, "missing-rounds-credential"), "chosen")
	listed, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range listed.Tools {
		if tool.Name != "review_rounds_configure" {
			continue
		}
		found = true
		raw, _ := json.Marshal(tool.InputSchema)
		if !strings.Contains(string(raw), `"enum":["grant","close","taper"]`) || !strings.Contains(string(raw), `"taper":{"maximum":1000,"minimum":1,"type":"integer"}`) {
			t.Fatal("discovery omitted the bounded taper action", string(raw))
		}
	}
	if !found {
		t.Fatal("round controls missing from discovery")
	}
	cases := map[string][]string{
		"review_rounds_read":      {`{}`, `null`, `[]`, `{"pull_number":2,"channel":"other"}`, `{"pull_number":2,"channel":"cli","project_id":"other"}`, `{"pull_number":2,"channel":"cli","minimum":0}`, `{"pull_number":2,"channel":"cli","channel":"hosted"}`},
		"review_rounds_configure": {`{}`, `{"pull_number":2,"channel":"cli","expected_revision":0,"idempotency_key":"once","action":"grant","maximum":3}`, `{"pull_number":2,"channel":"cli","expected_revision":0,"idempotency_key":"once","action":"grant","minimum":4,"maximum":3}`, `{"pull_number":2,"channel":"cli","expected_revision":0,"idempotency_key":"once","action":"close","minimum":0}`, `{"pull_number":2,"channel":"cli","expected_revision":null,"idempotency_key":"once","action":"close"}`},
	}
	for _, suffix := range []string{``, `,"taper":0`, `,"taper":1001`, `,"taper":null`, `,"taper":2,"minimum":0`, `,"taper":2,"maximum":8`, `,"taper":2,"taper":3`} {
		cases["review_rounds_configure"] = append(cases["review_rounds_configure"], `{"pull_number":2,"channel":"cli","expected_revision":1,"idempotency_key":"once","action":"taper"`+suffix+`}`)
	}
	cases["review_rounds_configure"] = append(cases["review_rounds_configure"], `{"pull_number":2,"channel":"cli","expected_revision":1,"idempotency_key":"once","action":"close","taper":2}`, `{"pull_number":2,"channel":"cli","expected_revision":1,"idempotency_key":"once","action":"grant","minimum":0,"maximum":8,"taper":2}`)
	for name, rows := range cases {
		for _, raw := range rows {
			diagnosticResult(t, call(t, session, name, json.RawMessage(raw)), "invalid_arguments", false)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid round arguments loaded credentials or contacted service")
	}
}

func TestReviewRoundsMCPActualAuthorityGrantCloseAndReplay(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t)
	server := httptest.NewServer(handler)
	defer server.Close()
	binding := profile.Context{Profile: "chosen", ServerURL: server.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	credential := filepath.Join(t.TempDir(), "admin.token")
	if err := os.WriteFile(credential, []byte(f.admin), 0600); err != nil {
		t.Fatal(err)
	}
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential), "chosen")
	var before client.ReviewRoundControl
	decodeResult(t, call(t, session, "review_rounds_read", map[string]any{"pull_number": 2, "channel": "cli"}), &before)
	if !before.Configured || before.Revision != 1 || before.Epoch != 1 || before.Minimum != 2 || before.Maximum != 6 || before.Taper != 2 || before.Used != 0 || before.Dry != 0 {
		t.Fatal("new MCP enrollment lost CLI defaults", before)
	}
	var initialHosted client.ReviewRoundControl
	decodeResult(t, call(t, session, "review_rounds_read", map[string]any{"pull_number": 2, "channel": "hosted"}), &initialHosted)
	// This fixture enrolls CLI only; defaults do not grant another channel.
	if initialHosted.Configured || initialHosted.Revision != 0 {
		t.Fatal("CLI-only enrollment granted Hosted", initialHosted)
	}
	grant := map[string]any{"pull_number": 2, "channel": "cli", "expected_revision": 1, "idempotency_key": "mcp-grant-once", "action": "grant", "minimum": 2, "maximum": 5}
	var receipt, replay client.ReviewRoundControlReceipt
	decodeResult(t, call(t, session, "review_rounds_configure", grant), &receipt)
	decodeResult(t, call(t, session, "review_rounds_configure", grant), &replay)
	if receipt != replay || receipt.Revision != 2 || receipt.Epoch != 1 || receipt.Outcome != "granted" {
		t.Fatal("grant replay changed receipt")
	}
	var configured client.ReviewRoundControl
	decodeResult(t, call(t, session, "review_rounds_read", map[string]any{"pull_number": 2, "channel": "cli"}), &configured)
	if !configured.Configured || configured.Minimum != 2 || configured.Maximum != 5 || configured.Taper != 2 || configured.Used != 0 {
		t.Fatal("grant read changed selected policy")
	}
	close := map[string]any{"pull_number": 2, "channel": "cli", "expected_revision": 2, "idempotency_key": "mcp-close-once", "action": "close"}
	decodeResult(t, call(t, session, "review_rounds_configure", close), &receipt)
	if receipt.Revision != 3 || receipt.Epoch != 1 || receipt.Outcome != "closed" {
		t.Fatal("close reset grant")
	}
	taper := map[string]any{"pull_number": 2, "channel": "cli", "expected_revision": 3, "idempotency_key": "mcp-taper-once", "action": "taper", "taper": 1}
	decodeResult(t, call(t, session, "review_rounds_configure", taper), &receipt)
	decodeResult(t, call(t, session, "review_rounds_configure", taper), &replay)
	if receipt != replay || receipt.Revision != 4 || receipt.Epoch != 1 || receipt.Outcome != "taper_updated" {
		t.Fatal("taper replay changed receipt", receipt, replay)
	}
	var changed client.ReviewRoundControl
	decodeResult(t, call(t, session, "review_rounds_read", map[string]any{"pull_number": 2, "channel": "cli"}), &changed)
	if changed.Taper != 1 || !changed.Closed || changed.Minimum != configured.Minimum || changed.Maximum != configured.Maximum || changed.Used != configured.Used || changed.Dry != configured.Dry || changed.Epoch != configured.Epoch {
		t.Fatal("taper changed explicit closure, grants or progress", changed)
	}
	var hosted client.ReviewRoundControl
	decodeResult(t, call(t, session, "review_rounds_read", map[string]any{"pull_number": 2, "channel": "hosted"}), &hosted)
	if hosted != initialHosted {
		t.Fatal("CLI closure changed Hosted defaults", hosted)
	}
	ordinary := reviewControlsStdio(t, reviewControlsProfile(t, f, server.URL), "chosen")
	decodeResult(t, call(t, ordinary, "review_rounds_configure", map[string]any{"pull_number": 2, "channel": "hosted", "expected_revision": 0, "idempotency_key": "nonadmin", "action": "close"}), &receipt)
	var count int
	if err := f.store.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM review_operations`).Scan(&count); err != nil || count != 1 {
		t.Fatal("controls dispatched or queued provider work", err, count)
	}
}
