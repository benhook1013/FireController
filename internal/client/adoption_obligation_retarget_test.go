package client_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/client"
)

func TestAdoptionObligationRetargetTypedTransportAndCurrentDetail(t *testing.T) {
	id := "adoption_obligation_" + strings.Repeat("a", 32)
	one, target := int64(1), int64(9)
	original := adoptionholdsmodel.Obligation{SourceKey: "route-one", Kind: "route", AffectedPullNumbers: []int64{2, 7}, Summary: "Receiving correction", Evidence: []string{"target-pr:7"}, OriginalStatus: "open"}
	expectedTargets := adoptionholdsmodel.RouteAffectedPullNumbers(original, target)
	current := adoptionholdsmodel.Detail{ID: id, ProjectID: "project-one", Source: adoptionholdsmodel.Source{Namespace: "original", RepositoryID: 7, SnapshotSHA256: strings.Repeat("b", 64)}, SourceRevision: 1, Original: original, AffectedPullNumbers: expectedTargets, Status: "open", Revision: 2, ActorID: "principal-one", WorkerID: "worker-one", CreatedAt: "2026-10-20T00:00:00Z", Retarget: &adoptionholdsmodel.Retarget{Revision: 2, Reason: "Receiver changed", ActorID: "principal-one", WorkerID: "worker-one", ChangedAt: "2026-10-21T00:00:00Z", TargetPullNumber: target}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("X-FC-Worker") != "worker-one" {
			t.Error("lost own worker binding")
		}
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/retarget"):
			var got client.AdoptionObligationRetargetRequest
			if json.NewDecoder(r.Body).Decode(&got) != nil || got.ExpectedRevision == nil || *got.ExpectedRevision != 1 || got.TargetPullNumber == nil || *got.TargetPullNumber != target || got.Reason != "Receiver changed" || r.Header.Get("Idempotency-Key") != "same-retarget" {
				t.Error("retarget request changed", got)
			}
			_ = json.NewEncoder(w).Encode(client.AdoptionObligationRetargetReceipt{ID: id, Status: "open", Revision: 2, TargetPullNumber: target, AffectedPullNumbers: expectedTargets, Outcome: "retargeted"})
		case r.Method == "GET":
			_ = json.NewEncoder(w).Encode(current)
		default:
			t.Error("unexpected provider or mutation request", r.Method, r.URL)
		}
	}))
	defer server.Close()
	c := newClient(t, server)
	receipt, err := c.RetargetAdoptionObligation(t.Context(), id, "same-retarget", client.AdoptionObligationRetargetRequest{ExpectedRevision: &one, TargetPullNumber: &target, Reason: "Receiver changed"})
	if err != nil || receipt.Revision != 2 || receipt.Status != "open" {
		t.Fatal(receipt, err)
	}
	got, err := c.AdoptionObligation(t.Context(), id)
	if err != nil || !reflect.DeepEqual(got, current) || !reflect.DeepEqual(got.Original, original) {
		t.Fatal("current scope or immutable origin lost", got, err)
	}
}

func TestAdoptionObligationRetargetValidationBeforeTransport(t *testing.T) {
	one, zero, bad := int64(1), int64(0), int64(-1)
	c := &client.Client{}
	for _, request := range []client.AdoptionObligationRetargetRequest{{ExpectedRevision: &one, Reason: "Missing receiver"}, {ExpectedRevision: &one, TargetPullNumber: &bad, Reason: "Bad receiver"}, {TargetPullNumber: &zero, Reason: "Missing revision"}, {ExpectedRevision: &one, TargetPullNumber: &zero}} {
		if _, err := c.RetargetAdoptionObligation(t.Context(), "adoption_obligation_"+strings.Repeat("a", 32), "key", request); err == nil {
			t.Fatal("invalid retarget reached transport", request)
		}
	}
	valid, err := client.NormalizeAdoptionObligationRetarget(client.AdoptionObligationRetargetRequest{ExpectedRevision: &one, TargetPullNumber: &zero, Reason: "Unassigned backlog"})
	if err != nil || valid.TargetPullNumber == nil || *valid.TargetPullNumber != 0 {
		t.Fatal("explicit zero rejected", valid, err)
	}
}

func TestAdoptionObligationRetargetRejectsUncertainReceiptsWithoutRetry(t *testing.T) {
	id := "adoption_obligation_" + strings.Repeat("a", 32)
	one, target := int64(1), int64(9)
	for _, mode := range []string{"lost", "wrong-target", "wrong-revision", "missing-scope", "unsorted-scope", "resolved"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				calls++
				if r.Method != "POST" || r.URL.Path != "/api/v1/projects/project-one/reviews/adoption-obligations/"+id+"/retarget" || r.Header.Get("Idempotency-Key") != "retain-key" || r.Header.Get("X-FC-Worker") != "worker-one" {
					t.Error("lost retarget binding")
				}
				if mode == "lost" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
					return
				}
				receipt := client.AdoptionObligationRetargetReceipt{ID: id, Status: "open", Revision: 2, TargetPullNumber: target, AffectedPullNumbers: []int64{2, 9}, Outcome: "retargeted"}
				switch mode {
				case "wrong-target":
					receipt.TargetPullNumber = 8
				case "wrong-revision":
					receipt.Revision = 1
				case "missing-scope":
					receipt.AffectedPullNumbers = []int64{2}
				case "unsorted-scope":
					receipt.AffectedPullNumbers = []int64{9, 2}
				case "resolved":
					receipt.Status = "resolved"
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(receipt)
			}))
			defer server.Close()
			c := newClient(t, server)
			_, err := c.RetargetAdoptionObligation(t.Context(), id, "retain-key", client.AdoptionObligationRetargetRequest{ExpectedRevision: &one, TargetPullNumber: &target, Reason: "Move receiver"})
			var failure *client.Error
			if !errors.As(err, &failure) || !failure.OutcomeUnknown || calls != 1 {
				t.Fatal("lost uncertain outcome or retried mutation", err, calls)
			}
		})
	}
}
