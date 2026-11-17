package mcpadapter

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewWaitWithinRemainingBudget(t *testing.T) {
	for _, tc := range []struct {
		requested int
		remaining time.Duration
		want      int
	}{{0, time.Minute, 0}, {2, 7500 * time.Millisecond, 2}, {50, 55 * time.Second, 50}, {50, 54999 * time.Millisecond, 49}, {50, 12750 * time.Millisecond, 7}, {2, 5 * time.Second, 0}, {50, time.Second, 0}, {50, -time.Second, 0}} {
		if got := reviewWaitWithinBudget(tc.requested, tc.remaining); got != tc.want {
			t.Fatal(tc, got)
		}
	}
}

func TestFitReviewWaitOnlyChangesScopedPreference(t *testing.T) {
	input := &operationStatusInput{ID: "original", WaitSeconds: 50, Diagnostics: true}
	original := arguments{reviewSourceWait: 50, operationStatus: input, key: "original-key", id: "original-id", reviewRequest: client.ReviewRequest{PullNumber: 2, Channel: "hosted", HeadSHA: "original-head", ExpectedConfigurationRevision: 3}}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	for _, name := range []string{"review_start", "review_selection_read", "review_request", "review_operation_read", "review_source_refresh", "review_checkpoint_publish", "review_checkpoint_recover"} {
		got := fitReviewWait(ctx, name, original)
		if got.reviewSourceWait != 0 || got.operationStatus == input || got.operationStatus.WaitSeconds != 0 || input.WaitSeconds != 50 {
			t.Fatal("wait did not fit or modified shared arguments", name, got)
		}
		got.reviewSourceWait, got.operationStatus = original.reviewSourceWait, original.operationStatus
		if !reflect.DeepEqual(got, original) {
			t.Fatal("wait changed request identity or other arguments", name)
		}
	}
	for _, name := range []string{"wait_for_messages", "job_read", "review_evidence_read"} {
		if got := fitReviewWait(ctx, name, original); !reflect.DeepEqual(got, original) {
			t.Fatal("unrelated operation changed", name)
		}
	}
	if got := fitReviewWait(context.Background(), "review_operation_read", original); !reflect.DeepEqual(got, original) {
		t.Fatal("deadline-free caller preference changed")
	}
	for _, seconds := range []int{0, 2} {
		args := original
		args.reviewSourceWait = seconds
		args.operationStatus = &operationStatusInput{WaitSeconds: seconds}
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		got := fitReviewWait(ctx, "review_operation_read", args)
		cancel()
		if !reflect.DeepEqual(got, args) {
			t.Fatal("fitting zero/default preference changed", seconds)
		}
	}
}
