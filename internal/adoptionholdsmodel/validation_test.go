package adoptionholdsmodel

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizationSeparatesUnknownOriginalEvidenceFromResolutionProof(t *testing.T) {
	zero := int64(0)
	batch := Batch{Version: 1, Source: Source{Namespace: "sealed-source", RepositoryID: 7, SnapshotSHA256: strings.Repeat("a", 64)}, ExpectedRevision: &zero, Obligations: []Obligation{
		{SourceKey: "z", Kind: "finding", AffectedPullNumbers: []int64{9, 2}, Summary: "retained finding", Evidence: []string{}, OriginalStatus: "open"},
		{SourceKey: "a", Kind: "route", AffectedPullNumbers: []int64{8}, Summary: "retained route", Evidence: []string{"private:pointer"}, OriginalStatus: "pending"},
	}}
	normalized, first, err := NormalizeBatch(batch)
	if err != nil || normalized.Obligations[0].SourceKey != "a" || !reflect.DeepEqual(normalized.Obligations[1].AffectedPullNumbers, []int64{2, 9}) || normalized.Obligations[1].Evidence == nil {
		t.Fatal("source normalization changed semantics", err, normalized)
	}
	again, second, err := NormalizeBatch(normalized)
	if err != nil || !reflect.DeepEqual(normalized, again) || string(first) != string(second) {
		t.Fatal("canonical normalization is not stable", err)
	}
	one := int64(1)
	for _, request := range []ResolveRequest{
		{ExpectedRevision: &one, Reason: "addressed", Evidence: nil},
		{ExpectedRevision: &one, Reason: "addressed", Evidence: []string{}},
	} {
		if _, err = NormalizeResolution(request); err == nil {
			t.Fatal("resolution without evidence admitted")
		}
	}
	request := ResolveRequest{ExpectedRevision: &one, Reason: "addressed", Evidence: []string{"native:proof"}}
	if got, err := NormalizeResolution(request); err != nil || !reflect.DeepEqual(got, request) {
		t.Fatal("bounded resolution refused", err, got)
	}
}

func TestNormalizedEncodedRecordBudgets(t *testing.T) {
	original := Obligation{SourceKey: "boundary", Kind: "finding", AffectedPullNumbers: []int64{1}, Summary: strings.Repeat("<", 5000), Evidence: []string{}, OriginalStatus: "pending"}
	original.Summary += strings.Repeat("x", (32<<10)-len(Encode(original)))
	if len(Encode(original)) != 32<<10 {
		t.Fatal("original fixture did not reach its encoded boundary")
	}
	if _, err := NormalizeObligation(original); err != nil {
		t.Fatal("exact original boundary refused", err)
	}
	original.Summary += "x"
	if _, err := NormalizeObligation(original); err == nil {
		t.Fatal("encoded original over boundary accepted")
	}
	one := int64(1)
	resolution := ResolveRequest{ExpectedRevision: &one, Reason: strings.Repeat("<", 2600), Evidence: []string{"x"}}
	resolution.Evidence[0] += strings.Repeat("x", (16<<10)-len(Encode(resolution)))
	if len(Encode(resolution)) != 16<<10 {
		t.Fatal("resolution fixture did not reach its encoded boundary")
	}
	if _, err := NormalizeResolution(resolution); err != nil {
		t.Fatal("exact resolution boundary refused", err)
	}
	resolution.Evidence[0] += "x"
	if _, err := NormalizeResolution(resolution); err == nil {
		t.Fatal("encoded resolution over boundary accepted")
	}
	for name, escaped := range map[string]string{"html": "<", "quote": `"`, "control": "\n"} {
		t.Run(name, func(t *testing.T) {
			original.Summary = strings.Repeat(escaped, (16<<10)-1) + "x"
			if _, err := NormalizeObligation(original); err == nil {
				t.Fatal("escaping bypassed original budget")
			}
			resolution.Reason = strings.Repeat(escaped, 4095) + "x"
			resolution.Evidence = make([]string, 10)
			for index := range resolution.Evidence {
				resolution.Evidence[index] = strings.Repeat(escaped, 1023) + "x"
			}
			if _, err := NormalizeResolution(resolution); err == nil {
				t.Fatal("escaping bypassed combined resolution budget")
			}
		})
	}
}

func TestResultKindRetainsCanonicalBoundsAndUnknownKindRefusal(t *testing.T) {
	for _, kind := range []string{"finding", "route", "result"} {
		if !ValidKind(kind) {
			t.Fatal("supported kind refused", kind)
		}
	}
	result := Obligation{SourceKey: "terminal-result", Kind: "result", AffectedPullNumbers: []int64{9, 2}, Summary: "Terminal result accounting remains unsettled", Evidence: []string{"source:terminal-result"}, OriginalStatus: "pending"}
	normalized, err := NormalizeObligation(result)
	if err != nil || normalized.Kind != "result" || !reflect.DeepEqual(normalized.AffectedPullNumbers, []int64{2, 9}) || !reflect.DeepEqual(result.AffectedPullNumbers, []int64{9, 2}) {
		t.Fatal("result normalization changed kind or caller scope", normalized, err)
	}
	for _, kind := range []string{"", "Result", "results", "instruction", " result"} {
		changed := result
		changed.Kind = kind
		if ValidKind(kind) {
			t.Fatal("unknown kind admitted", kind)
		}
		if _, err := NormalizeObligation(changed); err == nil {
			t.Fatal("unknown obligation kind normalized", kind)
		}
	}
	for name, change := range map[string]func(*Obligation){
		"summary":          func(o *Obligation) { o.Summary = strings.Repeat("x", (16<<10)+1) },
		"evidence-count":   func(o *Obligation) { o.Evidence = make([]string, 11) },
		"evidence-bytes":   func(o *Obligation) { o.Evidence = []string{strings.Repeat("x", 1025)} },
		"missing-evidence": func(o *Obligation) { o.Evidence = nil },
		"scope-count":      func(o *Obligation) { o.AffectedPullNumbers = make([]int64, 101) },
		"duplicate-scope":  func(o *Obligation) { o.AffectedPullNumbers = []int64{2, 2} },
		"scope-range":      func(o *Obligation) { o.AffectedPullNumbers = []int64{2147483648} },
	} {
		t.Run(name, func(t *testing.T) {
			changed := result
			change(&changed)
			if _, err := NormalizeObligation(changed); err == nil {
				t.Fatal("result bypassed shared obligation bound")
			}
		})
	}
	zero := int64(0)
	batch := Batch{Version: 1, Source: Source{Namespace: "sealed-source", RepositoryID: 7, SnapshotSHA256: strings.Repeat("a", 64)}, ExpectedRevision: &zero, Obligations: []Obligation{result}}
	if got, _, err := NormalizeBatch(batch); err != nil || got.Obligations[0].Kind != "result" {
		t.Fatal("bounded result batch refused", got, err)
	}
	batch.Obligations = make([]Obligation, 101)
	for index := range batch.Obligations {
		batch.Obligations[index] = result
		batch.Obligations[index].SourceKey = strings.Repeat("x", index+1)
	}
	if _, _, err := NormalizeBatch(batch); err == nil {
		t.Fatal("result batch count bypassed")
	}
	batch.Obligations = batch.Obligations[:20]
	for index := range batch.Obligations {
		batch.Obligations[index].Summary = strings.Repeat("x", 16<<10)
	}
	if _, _, err := NormalizeBatch(batch); err == nil {
		t.Fatal("result batch wrapper budget bypassed")
	}
}
