package reviewexec

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviews"
)

// Keep the original whole-array sizing as an independent behavioral oracle.
func wholeArrayHostedBatch(runs []reviews.RunInput, limit int) []reviews.RunInput {
	batch := []reviews.RunInput{}
	findings := 0
	for len(batch) < len(runs) && len(batch) < limit {
		candidate := runs[len(batch)]
		if findings+len(candidate.Findings) > 1000 {
			break
		}
		encoded, err := json.Marshal(append(batch, candidate))
		if err != nil || len(encoded) > 256<<10 {
			break
		}
		batch = append(batch, candidate)
		findings += len(candidate.Findings)
	}
	return batch
}

func TestExternalHostedInputBatchExactByteBudget(t *testing.T) {
	for _, count := range []int{1, 2, 100} {
		for _, delta := range []int{-1, 0, 1} {
			t.Run(fmt.Sprintf("runs%d/budget%+d", count, delta), func(t *testing.T) {
				runs := make([]reviews.RunInput, count)
				// Exercise UTF-8 byte lengths and JSON escaping, not just ASCII lengths.
				runs[count-1].Reviewer = "界🙂<>&\"\n\u2028"
				base, err := json.Marshal(runs)
				if err != nil {
					t.Fatal(err)
				}
				runs[count-1].Reviewer += strings.Repeat("x", (256<<10)+delta-len(base))
				encoded, err := json.Marshal(runs)
				if err != nil || len(encoded) != (256<<10)+delta {
					t.Fatal(len(encoded), err)
				}
				got := externalHostedInputBatch(runs, 100)
				want := count
				if delta > 0 {
					want--
				}
				if len(got) != want {
					t.Fatalf("batch length %d; want %d for %d bytes", len(got), want, len(encoded))
				}
				if !reflect.DeepEqual(got, wholeArrayHostedBatch(runs, 100)) {
					t.Fatal("whole-array budget changed")
				}
			})
		}
	}
}

func TestExternalHostedInputBatchLimitsMatchWholeArray(t *testing.T) {
	for _, tc := range []struct {
		name        string
		runs        []reviews.RunInput
		limit, want int
	}{
		{"nil", nil, 100, 0},
		{"empty", []reviews.RunInput{}, 100, 0},
		{"zero-limit", []reviews.RunInput{{}}, 0, 0},
		{"single", []reviews.RunInput{{}}, 100, 1},
		{"run-limit", make([]reviews.RunInput, 101), 100, 100},
		{"retry-limit", make([]reviews.RunInput, 10), 3, 3},
		{"oversized-first", []reviews.RunInput{{Reviewer: strings.Repeat("x", 256<<10)}, {}}, 100, 0},
		{"findings-exact", []reviews.RunInput{{Findings: make([]reviews.Finding, 500)}, {Findings: make([]reviews.Finding, 500)}, {}}, 100, 3},
		{"findings-over", []reviews.RunInput{{Findings: make([]reviews.Finding, 500)}, {Findings: make([]reviews.Finding, 501)}}, 100, 1},
		{"findings-first-over", []reviews.RunInput{{Findings: make([]reviews.Finding, 1001)}, {}}, 100, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := externalHostedInputBatch(tc.runs, tc.limit)
			if len(got) != tc.want {
				t.Fatalf("batch length %d; want %d", len(got), tc.want)
			}
			if !reflect.DeepEqual(got, wholeArrayHostedBatch(tc.runs, tc.limit)) {
				t.Fatal("whole-array budget changed")
			}
		})
	}
}

func BenchmarkExternalHostedInputBatch(b *testing.B) {
	for _, count := range []int{1, 10, 100} {
		runs := make([]reviews.RunInput, count)
		for i := range runs {
			runs[i].Reviewer = strings.Repeat("x", 2048)
		}
		for _, method := range []struct {
			name  string
			batch func([]reviews.RunInput, int) []reviews.RunInput
		}{{"incremental", externalHostedInputBatch}, {"whole-array", wholeArrayHostedBatch}} {
			b.Run(fmt.Sprintf("%s/%d", method.name, count), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if len(method.batch(runs, 100)) != count {
						b.Fatal("unexpected batch length")
					}
				}
			})
		}
	}
}
