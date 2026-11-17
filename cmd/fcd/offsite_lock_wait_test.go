package main

import (
	"bytes"
	"context"
	"testing"
)

func TestOffsiteBatchLockWaitIsExplicitAndBounded(t *testing.T) {
	for _, tc := range []struct {
		command, wait string
		want          int
	}{
		{"backup-push-batch", "0s", 3}, {"backup-push-batch", "2m", 3},
		{"backup-push-batch", "-1s", 2}, {"backup-push-batch", "121s", 2},
		{"backup-push", "1s", 2}, {"backup-offsite-status", "1s", 2},
	} {
		t.Run(tc.command+tc.wait, func(t *testing.T) {
			var out, diag bytes.Buffer
			got := runOffsiteCommand(context.Background(), tc.command, []string{"--config", t.TempDir() + "/absent.json", "--batch-lock-wait", tc.wait}, &out, &diag)
			if got != tc.want {
				t.Fatalf("exit %d want %d: %s", got, tc.want, diag.String())
			}
		})
	}
}
