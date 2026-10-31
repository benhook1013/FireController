package client_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/projectmap"
	"github.com/benhook1013/FireController/internal/publicsitemodel"
)

func linkedPublicationInput(t *testing.T) client.PublicationInput {
	t.Helper()
	in := publicationInput(t)
	in.Manifest.ProjectMap = []publicsitemodel.Card{{Key: "manual_map", Title: "Manual map", Phase: "Authored phase", Links: []publicsitemodel.Link{{Key: "guide", Label: "Guide", URL: "https://example.com/guide"}}}}
	in.Manifest.Progress = []publicsitemodel.Card{{Key: "manual_job", Title: "Manual progress", Description: "Keep manual prose", Status: "Editorial status", Links: []publicsitemodel.Link{}}}
	in.Manifest.WorkerLanes = []publicsitemodel.WorkerLane{{Key: "manual_lane", Label: "Editorial lane", Headline: "Manual headline", Milestones: []string{"Manual milestone"}}}
	for i, digit := range []string{"a", "b"} {
		key := []string{"one", "two"}[i]
		in.Manifest.JobRefs = append(in.Manifest.JobRefs, publicsitemodel.JobRef{Key: "job_" + key, JobID: "job_" + strings.Repeat(digit, 32), SummaryRevision: 1})
		in.Manifest.MapRefs = append(in.Manifest.MapRefs, publicsitemodel.MapRef{Key: "map_" + key, EntryID: "map_entry_" + strings.Repeat(digit, 32), Revision: 1})
		in.Manifest.WorkerRefs = append(in.Manifest.WorkerRefs, publicsitemodel.WorkerRef{Key: "worker_" + key, WorkerID: "worker_" + strings.Repeat(digit, 32), Label: "Public " + key})
	}
	raw, _ := json.Marshal(in)
	normalized, err := client.DecodePublicationInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	return normalized
}

func linkedPublicationPreview(t *testing.T, in client.PublicationInput) client.PublicationPreview {
	t.Helper()
	resolved := in.Manifest
	resolved.ProjectMap = append([]publicsitemodel.Card{}, in.Manifest.ProjectMap...)
	resolved.Progress = append([]publicsitemodel.Card{}, in.Manifest.Progress...)
	resolved.WorkerLanes = append([]publicsitemodel.WorkerLane{}, in.Manifest.WorkerLanes...)
	for i, ref := range in.Manifest.JobRefs {
		resolved.Progress = append(resolved.Progress, publicsitemodel.Card{Key: ref.Key, Title: []string{"Selected job 雪", "Second job"}[i], Description: "Approved summary", Status: "active", Links: []publicsitemodel.Link{}})
	}
	for _, ref := range in.Manifest.MapRefs {
		resolved.ProjectMap = append(resolved.ProjectMap, publicsitemodel.Card{Key: ref.Key, Title: "Selected map", Description: "Approved map", Status: "in progress", Links: []publicsitemodel.Link{}})
	}
	for i, ref := range in.Manifest.WorkerRefs {
		resolved.WorkerLanes = append(resolved.WorkerLanes, publicsitemodel.WorkerLane{Key: ref.Key, Label: ref.Label, Headline: resolved.Progress[len(in.Manifest.Progress)+i].Title, Status: "active", Milestones: []string{resolved.Progress[len(in.Manifest.Progress)+i].Title}})
	}
	resolved.JobRefs = nil
	resolved.MapRefs = nil
	resolved.WorkerRefs = nil
	content, err := publicsitemodel.BuildContent(resolved, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client.PublicationPreview{ExpectedRevision: in.ExpectedRevision, Enabled: in.Enabled, Manifest: in.Manifest, Content: content, SHA256: strings.Repeat("a", 64)}
}

func TestPublicationClientLinkedPreviewChecksEditorialPrefixAndReferenceTails(t *testing.T) {
	in := linkedPublicationInput(t)
	for name, change := range map[string]func(*client.PublicationPreview){
		"valid": func(*client.PublicationPreview) {},
		"valid-idle-worker": func(p *client.PublicationPreview) {
			p.Content.WorkerLanes[1].Headline = ""
			p.Content.WorkerLanes[1].Status = "idle"
			p.Content.WorkerLanes[1].Milestones = []string{}
		},
		"valid-server-resolved-prose": func(p *client.PublicationPreview) {
			p.Content.Progress[1].Title = "Authenticated resolved title"
			p.Content.Progress[1].Description = "Authenticated pinned prose"
			p.Content.WorkerLanes[1].Headline = p.Content.Progress[1].Title
			p.Content.WorkerLanes[1].Milestones = []string{p.Content.Progress[1].Title}
		},
		"manual-description": func(p *client.PublicationPreview) { p.Content.Description = "Changed" },
		"manual-map":         func(p *client.PublicationPreview) { p.Content.ProjectMap[0].Phase = "Changed" },
		"manual-progress":    func(p *client.PublicationPreview) { p.Content.Progress[0].Title = "Changed" },
		"manual-worker":      func(p *client.PublicationPreview) { p.Content.WorkerLanes[0].Label = "Changed" },
		"manifest":           func(p *client.PublicationPreview) { p.Manifest.JobRefs[0].SummaryRevision = 2 },
		"extra-map": func(p *client.PublicationPreview) {
			p.Content.ProjectMap = append(p.Content.ProjectMap, publicsitemodel.Card{Key: "extra", Title: "Extra", Links: []publicsitemodel.Link{}})
		},
		"missing-map": func(p *client.PublicationPreview) { p.Content.ProjectMap = p.Content.ProjectMap[:2] },
		"reordered-map": func(p *client.PublicationPreview) {
			p.Content.ProjectMap[1], p.Content.ProjectMap[2] = p.Content.ProjectMap[2], p.Content.ProjectMap[1]
		},
		"reordered-job": func(p *client.PublicationPreview) {
			p.Content.Progress[1], p.Content.Progress[2] = p.Content.Progress[2], p.Content.Progress[1]
		},
		"missing-job": func(p *client.PublicationPreview) { p.Content.Progress = p.Content.Progress[:2] },
		"extra-job": func(p *client.PublicationPreview) {
			p.Content.Progress = append(p.Content.Progress, publicsitemodel.Card{Key: "extra", Title: "Extra", Links: []publicsitemodel.Link{}})
		},
		"map-phase": func(p *client.PublicationPreview) { p.Content.ProjectMap[1].Phase = "Unselected" },
		"map-links": func(p *client.PublicationPreview) {
			p.Content.ProjectMap[1].Links = []publicsitemodel.Link{{Key: "extra", Label: "Extra", URL: "https://example.com"}}
		},
		"job-phase": func(p *client.PublicationPreview) { p.Content.Progress[1].Phase = "Unselected" },
		"job-links": func(p *client.PublicationPreview) {
			p.Content.Progress[1].Links = []publicsitemodel.Link{{Key: "extra", Label: "Extra", URL: "https://example.com"}}
		},
		"job-status":              func(p *client.PublicationPreview) { p.Content.Progress[1].Status = "unknown" },
		"job-oversized-summary":   func(p *client.PublicationPreview) { p.Content.Progress[1].Description = strings.Repeat("x", 2001) },
		"job-blank-summary":       func(p *client.PublicationPreview) { p.Content.Progress[1].Description = " " },
		"worker-label":            func(p *client.PublicationPreview) { p.Content.WorkerLanes[1].Label = "Unselected" },
		"worker-private-headline": func(p *client.PublicationPreview) { p.Content.WorkerLanes[1].Headline = "Unselected job" },
		"worker-wrong-status":     func(p *client.PublicationPreview) { p.Content.WorkerLanes[1].Status = "blocked" },
		"worker-private-milestone": func(p *client.PublicationPreview) {
			p.Content.WorkerLanes[1].Milestones = append(p.Content.WorkerLanes[1].Milestones, "Unselected job")
		},
		"worker-empty-milestones":       func(p *client.PublicationPreview) { p.Content.WorkerLanes[1].Milestones = []string{} },
		"worker-nonidle-empty-headline": func(p *client.PublicationPreview) { p.Content.WorkerLanes[1].Headline = "" },
		"extra-worker": func(p *client.PublicationPreview) {
			p.Content.WorkerLanes = append(p.Content.WorkerLanes, publicsitemodel.WorkerLane{Key: "extra", Label: "Unselected lane", Milestones: []string{}})
		},
		"reordered-worker": func(p *client.PublicationPreview) {
			p.Content.WorkerLanes[1], p.Content.WorkerLanes[2] = p.Content.WorkerLanes[2], p.Content.WorkerLanes[1]
		},
		"missing-worker": func(p *client.PublicationPreview) { p.Content.WorkerLanes = p.Content.WorkerLanes[:2] },
	} {
		t.Run(name, func(t *testing.T) {
			// Clone both request and response so corruption cannot mutate the assertion.
			raw, _ := json.Marshal(in)
			var selected client.PublicationInput
			_ = json.Unmarshal(raw, &selected)
			preview := linkedPublicationPreview(t, selected)
			change(&preview)
			body, _ := json.Marshal(preview)
			var calls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			c := newClient(t, srv)
			defer c.Close()
			got, err := c.PreviewPublication(t.Context(), in)
			if strings.HasPrefix(name, "valid") {
				if err != nil || !reflect.DeepEqual(got.Manifest, in.Manifest) {
					t.Fatal("valid linked preview refused", err)
				}
			} else {
				presentationClientError(t, err, "invalid_response", false)
			}
			if calls.Load() != 1 {
				t.Fatal("preview performed additional source reads")
			}
		})
	}
}

func TestPublicationClientActualLinkedPreviewApplyAndAnonymousProjection(t *testing.T) {
	f := publicationClientFixture(t)
	if err := projectmap.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	projectmap.Register(mux, f.store)
	f.handler = mux
	base := "/api/v1/projects/" + f.project
	job := f.call(t, "POST", base+"/jobs", f.admin, f.sender, map[string]any{"title": "PRIVATE_TITLE", "brief": "PRIVATE_BRIEF", "worker_id": f.sender})["id"].(string)
	f.call(t, "PUT", base+"/jobs/"+job+"/summary", f.admin, f.sender, map[string]any{"expected_job_revision": 1, "expected_revision": 0, "title": "Selected job 雪", "body": "Selected **summary**"})
	entry := f.call(t, "POST", base+"/map/entries", f.admin, f.sender, map[string]any{"kind": "workstream", "title": "Selected map", "description": "Selected map narrative", "state": "in progress", "where": "PRIVATE_WHERE", "next": "PRIVATE_NEXT", "links": []map[string]string{{"label": "Private", "url": "https://private.invalid"}}})["id"].(string)
	input := linkedPublicationInput(t)
	input.Manifest.JobRefs = input.Manifest.JobRefs[:1]
	input.Manifest.JobRefs[0].JobID = job
	input.Manifest.MapRefs = input.Manifest.MapRefs[:1]
	input.Manifest.MapRefs[0].EntryID = entry
	input.Manifest.WorkerRefs = input.Manifest.WorkerRefs[:1]
	input.Manifest.WorkerRefs[0].WorkerID = f.sender
	before := publicationClientCounts(t, f)
	providers := f.providerCalls.Load()
	srv := httptest.NewServer(f.handler)
	defer srv.Close()
	c := f.newClient(t, f.admin, f.sender, srv.URL)
	defer c.Close()
	preview, err := c.PreviewPublication(t.Context(), input)
	if err != nil {
		t.Fatal("actual linked preview refused", err)
	}
	if !reflect.DeepEqual(before, publicationClientCounts(t, f)) || !reflect.DeepEqual(preview.Manifest, input.Manifest) || preview.Content.Progress[1].Title != "Selected job 雪" || preview.Content.ProjectMap[1].Description != "Selected map narrative" || preview.Content.WorkerLanes[1].Headline != "Selected job 雪" {
		t.Fatal("linked projection changed selection or preview wrote state")
	}
	receipt, err := c.ApplyPublication(t.Context(), "linked-apply", preview.SHA256, input)
	if err != nil || receipt.Revision != 1 {
		t.Fatal(receipt, err)
	}
	before = publicationClientCounts(t, f)
	if replay, err := c.ApplyPublication(t.Context(), "linked-apply", preview.SHA256, input); err != nil || replay != receipt || !reflect.DeepEqual(before, publicationClientCounts(t, f)) {
		t.Fatal("linked replay changed state", err)
	}
	response, err := http.Get(srv.URL + "/api/v1/public/projects/" + input.Manifest.Slug)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode, string(body))
	}
	for _, secret := range []string{job, entry, f.sender, "job_refs", "map_refs", "worker_refs", "PRIVATE_", "private.invalid"} {
		if strings.Contains(string(body), secret) {
			t.Fatal("anonymous linked projection leaked source", secret)
		}
	}
	if f.providerCalls.Load() != providers {
		t.Fatal("publication performed provider work")
	}
}
