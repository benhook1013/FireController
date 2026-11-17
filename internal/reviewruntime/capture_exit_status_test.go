package reviewruntime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprocess"
)

func TestCaptureRetainsFullWindowsExitStatusWithoutCoverage(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("full unsigned Windows exit statuses require a 64-bit capture reader")
	}
	for _, code := range []int64{256, 0xc0000005, 0xffffffff} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := newCLIEvidenceFixture(t)
			attempt, workspace := f.startAdmitted(t, 2)
			capture := reviewprocess.Capture{Stdout: []byte{}, Stderr: []byte("synthetic crash output"), ExitCode: int(code), Started: true}
			started, finished := time.Now().UTC().Add(-time.Second), time.Now().UTC()
			got, err := RetainCLI(t.Context(), f.captures, f.engine, f.manager, attempt, workspace, capture, started, finished)
			if err != nil || got.Outcome != "failed" || got.Complete || got.Receipt.ID == "" {
				t.Fatal("valid crash status lost inspectable evidence", got, err)
			}
			_, reference, err := f.captures.Read(t.Context(), attempt.ID)
			if err != nil || reference.ID == "" {
				t.Fatal("crash capture could not be reopened", err)
			}
			raw, err := os.ReadFile(filepath.Join(f.captures.root, attempt.ID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Capture struct {
					ExitCode int64 `json:"exit_code"`
				} `json:"capture"`
			}
			if json.Unmarshal(raw, &envelope) != nil || envelope.Capture.ExitCode != code {
				t.Fatal("retention changed original exit status")
			}
			again, err := RetainCLI(t.Context(), f.captures, f.engine, f.manager, attempt, workspace, capture, started, finished)
			if err != nil || again != got || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence WHERE attempt_id=?`, attempt.ID) != 1 {
				t.Fatal("crash capture replay created another effect", again, err)
			}
		})
	}
	tooLarge := int64(0x100000000)
	if validCapture(reviewprocess.Capture{ExitCode: int(tooLarge)}) || validCapture(reviewprocess.Capture{ExitCode: -2}) {
		t.Fatal("invalid status crossed the capture boundary")
	}
}
