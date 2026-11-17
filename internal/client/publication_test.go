package client_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/publicsite"
	"github.com/benhook1013/FireController/internal/publicsitemodel"
)

func publicationInput(t *testing.T) client.PublicationInput {
	t.Helper()
	in, err := client.DecodePublicationInput([]byte(`{"expected_revision":0,"enabled":true,"manifest":{"slug":"published-demo","presentation":{"name":"Public <literal> project"},"description":"Approved description","reviews":[{"number":2,"title":"Approved PR title","url":"https://github.com/example/project/pull/2"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func publicationClientFixture(t *testing.T) *mutationFixture {
	t.Helper()
	f := newMutationFixture(t)
	if err := publicsite.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	publicsite.New(f.store, func(context.Context, *sql.Tx, string, publicsite.Manifest, string) ([]publicsite.ReviewFacts, error) {
		return []publicsite.ReviewFacts{{Number: 2, Lifecycle: "open", CLI: []publicsite.RoundFacts{{Key: "public_round_" + strings.Repeat("a", 32), Completed: true, Finalized: true, Complete: true, Found: 0}}}}, nil
	}).Register(mux, t.Context())
	f.handler = mux
	return f
}

func publicationClientCounts(t *testing.T, f *mutationFixture) []int64 {
	t.Helper()
	counts := make([]int64, 4)
	for i, table := range []string{"project_publications", "public_project_snapshots", "events", "receipts"} {
		if err := f.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&counts[i]); err != nil {
			t.Fatal(err)
		}
	}
	return counts
}

func TestPublicationClientActualPreviewReplayAndLostReceipt(t *testing.T) {
	f := publicationClientFixture(t)
	var lose atomic.Bool
	var applies atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/publication") {
			applies.Add(1)
			if r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("X-FC-Worker") != f.sender {
				t.Error("publication lost binding")
			}
			if lose.Swap(false) {
				record := httptest.NewRecorder()
				f.handler.ServeHTTP(record, r)
				if record.Code != 200 {
					t.Error("controlled request did not commit", record.Body.String())
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, "{")
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := f.newClient(t, f.admin, f.sender, srv.URL)
	initial, err := c.Publication(t.Context())
	if err != nil || initial.Revision != 0 || initial.Enabled || initial.Manifest != nil {
		t.Fatal(initial, err)
	}
	input := publicationInput(t)
	before, providers := publicationClientCounts(t, f), f.providerCalls.Load()
	preview, err := c.PreviewPublication(t.Context(), input)
	if err != nil || preview.Manifest.Slug != input.Manifest.Slug || preview.Content.Reviews[0].Title != "Approved PR title" || !reflect.DeepEqual(before, publicationClientCounts(t, f)) {
		t.Fatal("preview changed state or lost approved fields", preview, err)
	}
	anonymous := httptest.NewRecorder()
	f.handler.ServeHTTP(anonymous, httptest.NewRequest("GET", "/api/v1/public/projects/published-demo", nil))
	if anonymous.Code != 404 {
		t.Fatal("preview published data")
	}
	lose.Store(true)
	_, err = c.ApplyPublication(t.Context(), "first-key", preview.SHA256, input)
	presentationClientError(t, err, "invalid_response", true)
	if applies.Load() != 1 {
		t.Fatal("unknown publication retried")
	}
	before = publicationClientCounts(t, f)
	receipt, err := c.ApplyPublication(t.Context(), "first-key", preview.SHA256, input)
	if err != nil || receipt.Revision != 1 || receipt.Outcome != "updated" || !reflect.DeepEqual(before, publicationClientCounts(t, f)) {
		t.Fatal("exact recovery changed publication", receipt, err)
	}
	read, err := c.Publication(t.Context())
	if err != nil || read.Manifest == nil || !read.Enabled || read.Manifest.Presentation.Name != "Public <literal> project" {
		t.Fatal(read, err)
	}
	later := input
	later.ExpectedRevision = 1
	later.Manifest.Description = "Later explicit approval"
	p2, err := c.PreviewPublication(t.Context(), later)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ApplyPublication(t.Context(), "later-key", p2.SHA256, later); err != nil {
		t.Fatal(err)
	}
	before = publicationClientCounts(t, f)
	if replay, err := c.ApplyPublication(t.Context(), "first-key", preview.SHA256, input); err != nil || replay != receipt || !reflect.DeepEqual(before, publicationClientCounts(t, f)) {
		t.Fatal("later approval changed exact replay", replay, err)
	}
	unpublished, err := c.Unpublish(t.Context(), "disable-key", 2)
	if err != nil || unpublished.Revision != 3 || unpublished.Outcome != "updated" {
		t.Fatal(unpublished, err)
	}
	before = publicationClientCounts(t, f)
	if replay, err := c.Unpublish(t.Context(), "disable-key", 2); err != nil || replay != unpublished || !reflect.DeepEqual(before, publicationClientCounts(t, f)) {
		t.Fatal("unpublish recovery changed state", replay, err)
	}
	if f.providerCalls.Load() != providers {
		t.Fatal("publication invoked provider work")
	}
}

func TestPublicationClientStrictAdmissionAndSafeResponses(t *testing.T) {
	in := publicationInput(t)
	for _, raw := range []string{`{}`, `{"expected_revision":null,"enabled":true,"manifest":{}}`, `{"expected_revision":0,"enabled":null,"manifest":{}}`, `{"expected_revision":0,"enabled":true,"manifest":{"slug":"demo","presentation":{"name":"Public"}},"preview_sha256":"unexpected"}`, `{"expected_revision":0,"expected_revision":1,"enabled":true,"manifest":{}}`, strings.Repeat(" ", client.MaxPublicationDocumentBytes) + `{}`} {
		if _, err := client.DecodePublicationInput([]byte(raw)); err == nil {
			t.Fatal("invalid approved file admitted")
		}
	}
	var unloaded *client.Client
	if _, err := unloaded.ApplyPublication(t.Context(), "", strings.Repeat("a", 64), in); err == nil {
		t.Fatal("missing key reached client")
	}
	if _, err := unloaded.Unpublish(t.Context(), "key", -1); err == nil {
		t.Fatal("invalid revision reached client")
	}
	content, err := publicsitemodel.BuildContent(in.Manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	preview := client.PublicationPreview{ExpectedRevision: 0, Enabled: true, Manifest: in.Manifest, Content: content, SHA256: strings.Repeat("a", 64)}
	valid, _ := json.Marshal(preview)
	for name, body := range map[string]string{
		"unknown-field":           strings.Replace(string(valid), `"expected_revision":0`, `"expected_revision":0,"private_actor":"sentinel"`, 1),
		"wrong-enabled":           strings.Replace(string(valid), `"enabled":true`, `"enabled":false`, 1),
		"wrong-revision":          strings.Replace(string(valid), `"expected_revision":0`, `"expected_revision":1`, 1),
		"wrong-content-editorial": strings.Replace(string(valid), `"description":"Approved description"`, `"description":"Different title"`, 2),
		"unsafe-content-url":      strings.Replace(string(valid), `https://github.com/example/project/pull/2`, `javascript:alert(1)`, 2),
		"null-content":            strings.Replace(string(valid), `"content":{`, `"content":null,"extra":{`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			c := newClient(t, srv)
			defer c.Close()
			_, err := c.PreviewPublication(t.Context(), in)
			presentationClientError(t, err, "invalid_response", false)
		})
	}
	for _, body := range []string{`{"revision":1,"outcome":"updated","private_actor":"sentinel"}`, `{"revision":0,"outcome":"updated"}`, `{"revision":2,"outcome":"updated"}`, `{"revision":1,"outcome":"unchanged"}`, `{"revision":null,"outcome":"updated"}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/instance" {
				handshake(w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}))
		c := newClient(t, srv)
		_, err := c.ApplyPublication(t.Context(), "key", strings.Repeat("a", 64), in)
		presentationClientError(t, err, "invalid_response", true)
		c.Close()
		srv.Close()
	}
}

func TestPublicationClientRequiresAdministratorWithoutPublicationEffects(t *testing.T) {
	f := publicationClientFixture(t)
	token := "synthetic-publication-nonadmin-token-1234567890"
	sum := sha256.Sum256([]byte(token))
	f.call(t, "POST", "/api/v1/projects/"+f.project+"/credentials", f.admin, "", map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": []string{"read", "write"}})
	srv := httptest.NewServer(f.handler)
	defer srv.Close()
	c := f.newClient(t, token, f.sender, srv.URL)
	before := publicationClientCounts(t, f)
	_, err := c.Publication(t.Context())
	presentationClientError(t, err, "forbidden", false)
	_, err = c.PreviewPublication(t.Context(), publicationInput(t))
	presentationClientError(t, err, "forbidden", false)
	_, err = c.ApplyPublication(t.Context(), "key", strings.Repeat("a", 64), publicationInput(t))
	presentationClientError(t, err, "forbidden", false)
	_, err = c.Unpublish(t.Context(), "key", 0)
	presentationClientError(t, err, "forbidden", false)
	if !reflect.DeepEqual(before, publicationClientCounts(t, f)) {
		t.Fatal("nonadministrator request changed publication")
	}
}

func TestPublicationClientCancellationDistinguishesReadAndAcceptedMutation(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		t.Run(map[bool]string{false: "preview", true: "apply"}[mutate], func(t *testing.T) {
			started, release := make(chan struct{}, 1), make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				started <- struct{}{}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer srv.Close()
			defer close(release)
			c := newClient(t, srv)
			defer c.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			in := publicationInput(t)
			go func() {
				if mutate {
					_, err := c.ApplyPublication(ctx, "key", strings.Repeat("a", 64), in)
					done <- err
				} else {
					_, err := c.PreviewPublication(ctx, in)
					done <- err
				}
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("publication request did not reach handler")
			}
			cancel()
			var err error
			select {
			case err = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("publication request ignored cancellation")
			}
			var failure *client.Error
			if !errors.As(err, &failure) || failure.Code != "canceled" || failure.OutcomeUnknown != mutate {
				t.Fatal("cancel changed outcome semantics", err)
			}
		})
	}
}
