package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCIBatchingFlagIsExplicitAndAccepted(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), []string{"--lifecycle-ci-batching=" + value}, &stdout, &stderr)
		if code != 2 || stdout.Len() != 0 || strings.Contains(stderr.String(), "flag provided but not defined") {
			t.Fatalf("flag %s did not reach ordinary validation: code %d", value, code)
		}
	}
}

func TestCIQualificationFlagIsExplicitAndAccepted(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), []string{"--lifecycle-ci-batching-qualification=" + value}, &stdout, &stderr)
		if code != 2 || stdout.Len() != 0 || strings.Contains(stderr.String(), "flag provided but not defined") {
			t.Fatalf("flag %s code%d", value, code)
		}
	}
}

func TestMetadataBatchingFlagIsExplicitAndAccepted(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), []string{"--lifecycle-metadata-batching=" + value}, &stdout, &stderr)
		if code != 2 || stdout.Len() != 0 || strings.Contains(stderr.String(), "flag provided but not defined") {
			t.Fatalf("metadata flag %s did not reach ordinary validation: code %d", value, code)
		}
	}
}
