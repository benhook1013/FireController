package adoptionholds_test

import (
	"context"
	"encoding/json"
	"fmt"
	h "github.com/benhook1013/FireController/internal/adoptionholds"
	m "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"os"
	"runtime"
	"sort"
	"testing"
	"time"
)

// Opt-in synthetic measurement; no serving state or provider operations.
func TestSyntheticResolvedHistoryAdmissionCost(t *testing.T) {
	if os.Getenv("FC_ADOPTION_COST_BENCH") != "1" || testing.Short() {
		t.Skip("opt-in synthetic scaling measurement")
	}
	for _, scenario := range []struct{ n, depth int }{{0, 1}, {100, 1}, {1000, 1}, {5000, 1}, {1000, 5}, {1000, 20}} {
		t.Run(fmt.Sprintf("records%d-depth%d", scenario.n, scenario.depth), func(t *testing.T) {
			f := fresh(t)
			targets := make([]int64, scenario.depth)
			for i := range targets {
				targets[i] = 3251 + int64(i)
			}
			setup := time.Now()
			for offset := 0; offset < scenario.n; offset += 100 {
				b := batch()
				b.Source.Namespace = fmt.Sprintf("cost-source-%04d", offset/100)
				b.Obligations = nil
				for j := offset; j < offset+100 && j < scenario.n; j++ {
					b.Obligations = append(b.Obligations, m.Obligation{SourceKey: fmt.Sprintf("cost-%05d", j), Kind: "finding", AffectedPullNumbers: targets, Summary: "Synthetic retained history", Evidence: []string{}, OriginalStatus: "open"})
				}
				f.apply(b, fmt.Sprintf("cost-apply-%d", offset))
			}
			rows, err := f.store.DB().Query(`SELECT id FROM review_adoption_obligations ORDER BY id`)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			one := int64(1)
			for i, id := range ids {
				decode[map[string]any](t, f.call("POST", base+"/"+id+"/resolve", f.token, "worker_one", fmt.Sprintf("cost-resolve-%d", i), m.ResolveRequest{ExpectedRevision: &one, Reason: "Synthetic resolved fixture", Evidence: []string{"synthetic:proof"}}), 200)
			}
			setupMS := float64(time.Since(setup).Microseconds()) / 1000
			// Warm once, then independently revalidate each ancestor on each sample.
			for _, pull := range targets {
				if open, err := h.HasOpen(t.Context(), f.store.DB(), "project_one", pull); err != nil || open {
					t.Fatal(open, err)
				}
			}
			var durations []float64
			var allocated uint64
			var counted *obligationCountingQuery
			for sample := 0; sample < 5; sample++ {
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				q := &obligationCountingQuery{DB: f.store.DB()}
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				start := time.Now()
				for _, pull := range targets {
					if open, err := h.HasOpen(ctx, q, "project_one", pull); err != nil || open {
						cancel()
						t.Fatal(open, err)
					}
				}
				durations = append(durations, float64(time.Since(start).Microseconds())/1000)
				cancel()
				runtime.ReadMemStats(&after)
				allocated += after.TotalAlloc - before.TotalAlloc
				counted = q
			}
			sort.Float64s(durations)
			out := map[string]any{"records": scenario.n, "ancestors": scenario.depth, "samples": 5, "min_ms": durations[0], "median_ms": durations[2], "max_ms": durations[4], "mean_allocated_bytes": allocated / 5, "original_reads": counted.originals, "target_reads": counted.targets, "resolution_reads": counted.resolutions, "source_reads": counted.source, "setup_ms": setupMS, "go": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0), "num_cpu": runtime.NumCPU()}
			b, _ := json.Marshal(out)
			t.Log("ADMISSION_COST " + string(b))
		})
	}
}
