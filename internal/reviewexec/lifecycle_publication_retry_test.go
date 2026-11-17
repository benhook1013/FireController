package reviewexec

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestLifecyclePublicationOverloadReducesBatchWithoutRepeatingSource(t *testing.T) {
	var limits []int
	err := retryLifecyclePublication(t.Context(), func(limit int) error {
		limits = append(limits, limit)
		if limit > 25 {
			return core.Fail("overloaded", "bounded writer expired", 503)
		}
		return nil
	})
	if err != nil || !reflect.DeepEqual(limits, []int{100, 50, 25}) {
		t.Fatal(limits, err)
	}
}

func TestLifecyclePublicationCanDeferExternalResultsAndPreserveMetadata(t *testing.T) {
	var limits []int
	err := retryLifecyclePublication(t.Context(), func(limit int) error {
		limits = append(limits, limit)
		if limit != 0 {
			return core.Fail("overloaded", "bounded writer expired", 503)
		}
		return nil
	})
	if err != nil || !reflect.DeepEqual(limits, []int{100, 50, 25, 12, 6, 3, 1, 0}) {
		t.Fatal(limits, err)
	}
}

func TestLifecyclePublicationKeepsStorageErrorsAndCancellation(t *testing.T) {
	want := errors.New("storage failure")
	calls := 0
	if err := retryLifecyclePublication(t.Context(), func(int) error { calls++; return want }); !errors.Is(err, want) || calls != 1 {
		t.Fatal(calls, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	err := retryLifecyclePublication(ctx, func(int) error {
		cancel()
		return core.Fail("overloaded", "cancelled", 503)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
