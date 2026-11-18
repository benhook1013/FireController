package client_test

import (
	"errors"
	"github.com/benhook1013/FireController/internal/client"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOwnerReviewPermissionReceiptRequiresExactElements(t *testing.T) {
	for _, permissions := range [][]string{{"read,write"}, {"read", "write"}} {
		service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/instance" {
				handshake(w)
				return
			}
			write(w, 200, client.ProjectCredentialUpdateReceipt{ID: "credential-one", ProjectID: "project-one", Outcome: "updated", Permissions: permissions})
		}))
		c := newClient(t, service)
		_, err := c.UpdateProjectCredential(t.Context(), "update-key", "credential-one", client.UpdateProjectCredentialRequest{ExpectedPermissions: []string{"read"}, Permissions: []string{"read", "write"}})
		if len(permissions) == 1 {
			var e *client.Error
			if !errors.As(err, &e) || !e.OutcomeUnknown || e.Code != "invalid_response" {
				t.Errorf("malformed receipt accepted: %v", err)
			}
		} else if err != nil {
			t.Error(err)
		}
		service.Close()
	}
}
func TestOwnerReviewActiveWorkerPageRejectsRetiredEntries(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		write(w, 200, client.Page[client.Worker]{Items: []client.Worker{{ID: "worker-old", ProjectID: "project-one", Name: "Old", Retired: true}}})
	}))
	defer service.Close()
	c := newClient(t, service)
	_, err := c.Workers(t.Context(), client.PageOptions{})
	checkError(t, err, "invalid_response")
	page, err := c.WorkersWithRetired(t.Context(), client.PageOptions{}, true)
	if err != nil || len(page.Items) != 1 || !page.Items[0].Retired {
		t.Fatal(page, err)
	}
}
