package reviewexec

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHostedCooldownQueuePriorityProjection(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	lower := f.enqueue(t, attemptProject, "display-later-first", "hosted", 4, 1)
	due := priorityCooldown(t, f, lower, 4, 3*time.Minute)
	head := f.enqueue(t, attemptProject, "display-head-second", "hosted", 2, 1)
	priorityCooldown(t, f, head, 2, 4*time.Minute)
	f.e.schedulingNow = func() time.Time { return due.Add(-time.Minute) }
	path := roundProgressPath(attemptProject) + "?pull_numbers=4,2&view=stages"
	read := func(version string) RoundProgress {
		t.Helper()
		response := versionWaitRead(t, f, path, version)
		var result RoundProgress
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Items) != 2 {
			t.Fatal("missing displayed rows", result)
		}
		return result
	}
	before := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_operation_waits") + snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "receipts") + snapshotTable(t, f, "events")
	result := read("5")
	lowerWait, headWait := result.Items[0].Channels[0].Wait, result.Items[1].Channels[0].Wait
	if lowerWait == nil || headWait == nil || lowerWait.QueueAheadPullNumber != 2 || headWait.QueueAheadPullNumber != 0 || lowerWait.RetryNotBefore != due.Format(time.RFC3339Nano) {
		t.Fatal("timer masked actual accepted priority", result)
	}
	responseWithActivity := versionWaitRead(t, f, path+"&include_activity_evidence=true", "5")
	var withActivity RoundProgress
	if err := json.Unmarshal(responseWithActivity.Body.Bytes(), &withActivity); err != nil {
		t.Fatal(err)
	}
	if len(withActivity.Items) != 2 || withActivity.Items[0].Channels[0].Wait == nil || withActivity.Items[0].Channels[0].Wait.QueueAheadPullNumber != 2 {
		t.Fatal("activity opt-in discarded queue priority")
	}
	for _, version := range []string{"1", "2", "3", "4"} {
		old := read(version)
		if old.Items[0].Channels[0].Wait == nil || old.Items[0].Channels[0].Wait.QueueAheadPullNumber != 0 {
			t.Fatal("legacy wait shape changed", version, old)
		}
		if strings.Contains(versionWaitRead(t, f, path, version).Body.String(), "queue_ahead_pull_number") {
			t.Fatal("queue hint escaped negotiation", version)
		}
	}
	if after := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_operation_waits") + snapshotTable(t, f, "review_execution_attempts") + snapshotTable(t, f, "receipts") + snapshotTable(t, f, "events"); before != after {
		t.Fatal("presentation read changed retained work")
	}
	timerControl(t, f, head.ID, "pause", "display-pause-head")
	if read("5").Items[0].Channels[0].Wait.QueueAheadPullNumber != 0 {
		t.Fatal("paused earlier request reserved display priority")
	}
	timerControl(t, f, head.ID, "cancel", "display-cancel-head")
	if read("5").Items[0].Channels[0].Wait.QueueAheadPullNumber != 0 {
		t.Fatal("mere earlier queue row reserved display priority")
	}
}

func TestQueuePriorityWaitVersionsRemainOrthogonal(t *testing.T) {
	source := &SourceRetry{}
	allowance := &AllowanceObservation{}
	original := &OperationWait{SourceRetry: source, CLIAllowance: allowance, QueueAheadPullNumber: 4}
	v4 := operationWaitForResponse(original, "cli", "4")
	if v4.SourceRetry != source || v4.CLIAllowance != nil || v4.QueueAheadPullNumber != 0 {
		t.Fatal("version 4 changed", v4)
	}
	v5 := operationWaitForResponse(original, "hosted", "5")
	if v5.SourceRetry != nil || v5.CLIAllowance != allowance || v5.QueueAheadPullNumber != 4 {
		t.Fatal("version 5 is not the quota/priority projection", v5)
	}
	if original.SourceRetry != source || original.CLIAllowance != allowance || original.QueueAheadPullNumber != 4 {
		t.Fatal("projection mutated retained input")
	}
}
