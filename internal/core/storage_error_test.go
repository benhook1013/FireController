package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type storageTestCode int

func (e storageTestCode) Error() string { return "private driver diagnostic" }
func (e storageTestCode) Code() int     { return int(e) }

func TestNormalizeStorageError(t *testing.T) {
	if NormalizeStorageError(nil) != nil {
		t.Fatal("nil became a failure")
	}
	for _, code := range []string{"overloaded", "storage_unavailable", "repository_changed"} {
		original := fmt.Errorf("wrapped: %w", Fail(code, "Safe domain failure.", 503))
		if NormalizeStorageError(original) != original {
			t.Fatal("existing domain classification changed", code)
		}
	}
	for _, tc := range []struct {
		err  error
		code string
	}{
		{context.Canceled, "overloaded"},
		{context.DeadlineExceeded, "overloaded"},
		{storageTestCode(5), "overloaded"},
		{storageTestCode(6), "overloaded"},
		{storageTestCode(517), "overloaded"},
		{storageTestCode(262), "overloaded"},
		{storageTestCode(1), "storage_unavailable"},
		{errors.New("private unknown failure"), "storage_unavailable"},
	} {
		err := NormalizeStorageError(fmt.Errorf("private wrapper: %w", tc.err))
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != tc.code || strings.Contains(err.Error(), "private") {
			t.Fatal("unsafe or incorrect storage classification", err)
		}
	}
}
