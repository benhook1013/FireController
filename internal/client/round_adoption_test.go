package client_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func adoptionDocument() client.PreparedReviewRoundControl {
	zero := int64(0)
	return client.PreparedReviewRoundControl{Version: 1, RepositoryID: 123, ExpectedRevision: &zero, PullNumber: 2, Channel: "cli", Source: "selected source", SourceKey: "2:cli", SourceSHA256: strings.Repeat("a", 64), MinimumRemaining: 0, MaximumRemaining: 4, InitialDry: 2, Taper: 3}
}
func adoptionRaw(t *testing.T) []byte {
	t.Helper()
	b, e := json.Marshal(adoptionDocument())
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func TestRoundAdoptionDocumentStrictBeforeAnyService(t *testing.T) {
	good := adoptionRaw(t)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(good, &fields); err != nil {
		t.Fatal(err)
	}
	for name := range fields {
		for _, mode := range []string{"missing", "null"} {
			t.Run(name+"-"+mode, func(t *testing.T) {
				clone := map[string]json.RawMessage{}
				for k, v := range fields {
					clone[k] = v
				}
				if mode == "missing" {
					delete(clone, name)
				} else {
					clone[name] = json.RawMessage("null")
				}
				raw, _ := json.Marshal(clone)
				if client.ValidateReviewRoundAdoptionDocument(raw) == nil {
					t.Fatal("accepted incomplete provenance/control")
				}
			})
		}
	}
	for _, raw := range [][]byte{[]byte("null"), append(good[:len(good)-1], []byte(`,"closed":false}`)...), append(good[:len(good)-1], []byte(`,"unknown":0}`)...), []byte(strings.Repeat(" ", 16<<10) + string(good))} {
		if client.ValidateReviewRoundAdoptionDocument(raw) == nil {
			t.Fatal("accepted ambiguous/oversized prepared control")
		}
	}
	for _, change := range []func(*client.PreparedReviewRoundControl){
		func(v *client.PreparedReviewRoundControl) { v.MaximumRemaining = -1 }, func(v *client.PreparedReviewRoundControl) { v.MinimumRemaining = 5 }, func(v *client.PreparedReviewRoundControl) { v.InitialDry = 1001 }, func(v *client.PreparedReviewRoundControl) { v.Taper = 0 },
		func(v *client.PreparedReviewRoundControl) { v.InitialDry = v.Taper; v.Tapered = false }, func(v *client.PreparedReviewRoundControl) { v.Taper = 1001 }, func(v *client.PreparedReviewRoundControl) { v.SourceKey = " key " }, func(v *client.PreparedReviewRoundControl) { v.SourceSHA256 = strings.Repeat("A", 64) }, func(v *client.PreparedReviewRoundControl) { one := int64(1); v.ExpectedRevision = &one },
	} {
		v := adoptionDocument()
		change(&v)
		b, _ := json.Marshal(v)
		if client.ValidateReviewRoundAdoptionDocument(b) == nil {
			t.Fatal("accepted unsupported configuration")
		}
	}
	for _, change := range []func(*client.PreparedReviewRoundControl){func(v *client.PreparedReviewRoundControl) { v.MaximumRemaining = 0 }, func(v *client.PreparedReviewRoundControl) { v.Tapered = true; v.InitialDry = 0 }, func(v *client.PreparedReviewRoundControl) { v.Taper = 8; v.InitialDry = 7 }, func(v *client.PreparedReviewRoundControl) { v.MinimumRemaining = 4; v.Closed = true }} {
		v := adoptionDocument()
		change(&v)
		b, _ := json.Marshal(v)
		if e := client.ValidateReviewRoundAdoptionDocument(b); e != nil {
			t.Fatal("lost supported effective rule", e)
		}
	}
}

func TestRoundAdoptionSingleDispatchAndStrictReceipt(t *testing.T) {
	for _, mode := range []string{"good", "foreign-preview", "changed-payload", "bad-receipt", "lost-result"} {
		t.Run(mode, func(t *testing.T) {
			var posts atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/instance") {
					handshake(w)
					return
				}
				if r.Method != "POST" || r.Header.Get("X-FC-Worker") != "worker-one" {
					t.Error("lost bound admin request")
				}
				posts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/preview") {
					var control client.PreparedReviewRoundControl
					if json.NewDecoder(r.Body).Decode(&control) != nil || control.PullNumber != 2 || control.InitialDry != 2 || r.Header.Get("Idempotency-Key") != "" {
						t.Error("rewrote source/control")
					}
					// Hash canonical wire bytes exactly as the service does.
					b, _ := json.Marshal(control)
					sha := adoptionHash(b)
					p := client.ReviewRoundAdoptionPreview{ProjectID: "project-one", PullNumber: 2, Channel: "cli", PayloadSHA256: sha, PreviewSHA256: strings.Repeat("b", 64)}
					if mode == "foreign-preview" {
						p.ProjectID = "other"
					}
					if mode == "changed-payload" {
						p.PayloadSHA256 = strings.Repeat("c", 64)
					}
					_ = json.NewEncoder(w).Encode(p)
					return
				}
				if r.URL.Path != "/api/v1/projects/project-one/reviews/rounds/adoption/apply" || r.Header.Get("Idempotency-Key") != "adopt-once" {
					t.Error("wrong adoption route/key")
				}
				var body struct {
					Preview string                            `json:"preview_sha256"`
					Control client.PreparedReviewRoundControl `json:"control"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Preview != strings.Repeat("b", 64) || body.Control.InitialDry != 2 {
					t.Error("lost original preview")
				}
				if mode == "lost-result" {
					conn, _, e := w.(http.Hijacker).Hijack()
					if e == nil {
						_ = conn.Close()
					}
					return
				}
				receipt := client.ReviewRoundControlReceipt{PullNumber: 2, Channel: "cli", Revision: 1, Epoch: 1, Outcome: "adopted"}
				if mode == "bad-receipt" {
					receipt.Epoch = 2
				}
				_ = json.NewEncoder(w).Encode(receipt)
			}))
			defer srv.Close()
			c := newClient(t, srv)
			raw := adoptionRaw(t)
			p, e := c.PreviewReviewRoundAdoption(context.Background(), raw)
			if mode == "foreign-preview" || mode == "changed-payload" {
				if e == nil || posts.Load() != 1 {
					t.Fatal("accepted invalid preview", e)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			result, e := c.ApplyReviewRoundAdoption(context.Background(), "adopt-once", p.PreviewSHA256, raw)
			if posts.Load() != 2 {
				t.Fatal("retried dispatched adoption", posts.Load())
			}
			if mode == "good" {
				if e != nil || result.Outcome != "adopted" {
					t.Fatal(result, e)
				}
				return
			}
			var failure *client.Error
			if !errors.As(e, &failure) || !failure.OutcomeUnknown {
				t.Fatal("lost uncertain mutation outcome", e)
			}
		})
	}
}

func adoptionHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
