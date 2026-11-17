package publicsitemodel

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/presentationmodel"
)

func TestLinkedReferenceValidationAndLegacyWire(t *testing.T) {
	legacy := Manifest{Slug: "project", Presentation: presentationmodel.Config{Name: "Project"}, Description: "Description"}
	normalized, err := NormalizeManifest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"slug":"project","presentation":{"name":"Project","brand_name":"FireController","tagline":"Project workspace","headings":{},"theme":{}},"description":"Description","links":[],"project_map":[],"progress":[],"worker_lanes":[],"reviews":[]}`
	if string(raw) != want {
		t.Fatalf("legacy no-reference wire changed:\n%s", raw)
	}
	valid := legacy
	valid.JobRefs = []JobRef{{Key: "job", JobID: "job_" + strings.Repeat("a", 32), SummaryRevision: 1}}
	valid.MapRefs = []MapRef{{Key: "map", EntryID: "map_entry_" + strings.Repeat("b", 32), Revision: 2}}
	valid.WorkerRefs = []WorkerRef{{Key: "worker", WorkerID: "worker_" + strings.Repeat("c", 32), Label: "Public worker"}}
	if _, err = NormalizeManifest(valid); err != nil {
		t.Fatal("valid references refused", err)
	}
	for name, mutate := range map[string]func(*Manifest){
		"job id":       func(m *Manifest) { m.JobRefs[0].JobID = "private-job" },
		"map id":       func(m *Manifest) { m.MapRefs[0].EntryID = "map_entry_bad" },
		"worker id":    func(m *Manifest) { m.WorkerRefs[0].WorkerID = "worker_bad" },
		"revision":     func(m *Manifest) { m.JobRefs[0].SummaryRevision = 0 },
		"public label": func(m *Manifest) { m.WorkerRefs[0].Label = strings.Repeat("x", 121) },
		"global key":   func(m *Manifest) { m.MapRefs[0].Key = "job" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.JobRefs = append([]JobRef(nil), valid.JobRefs...)
			candidate.MapRefs = append([]MapRef(nil), valid.MapRefs...)
			candidate.WorkerRefs = append([]WorkerRef(nil), valid.WorkerRefs...)
			mutate(&candidate)
			if _, err := NormalizeManifest(candidate); err == nil {
				t.Fatal("invalid linked reference accepted")
			}
		})
	}
	over := legacy
	over.Progress = make([]Card, MaxCards)
	over.JobRefs = valid.JobRefs
	if _, err = NormalizeManifest(over); err == nil {
		t.Fatal("combined static and linked card limit was not enforced")
	}
}
