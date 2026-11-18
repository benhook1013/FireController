package cli

import (
	"context"
	"errors"
	"github.com/benhook1013/FireController/internal/client"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOwnerReviewInboxOptionValues(t *testing.T) {
	for _, args := range [][]string{
		{"inbox", "list", "--worker", "--unread"},
		{"inbox", "ack", "message-one", "--key", "--worker"},
		{"inbox", "ack", "message-one", "--key", "good", "--worker", "--key"},
	} {
		if _, ok := parseInvocation(args); ok {
			t.Errorf("accepted missing option value: %v", args)
		}
	}
	for _, args := range [][]string{
		{"inbox", "list", "--worker=--unread"},
		{"inbox", "ack", "message-one", "--key=--key", "--worker=--worker"},
	} {
		if _, ok := parseInvocation(args); !ok {
			t.Errorf("rejected explicit value: %v", args)
		}
	}
}
func TestOwnerReviewPrivateInputDiagnostics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, in := range []*ownerManagementInvocation{{credentialFile: "/missing"}, {file: "/missing"}} {
		if err := prepareOwnerManagement(ctx, in); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation replaced: %v", err)
		}
	}
	dir := t.TempDir()
	public := filepath.Join(dir, "public.json")
	if err := os.WriteFile(public, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(dir, "missing"), dir}
	if runtime.GOOS != "windows" {
		paths = append(paths, public)
	}
	for _, path := range paths {
		err := prepareOwnerManagement(context.Background(), &ownerManagementInvocation{file: path})
		var e *client.Error
		if !errors.As(err, &e) || e.Code != "invalid_management_input" {
			t.Fatalf("wrong local diagnostic for %s: %v", path, err)
		}
	}
}
func TestOwnerReviewGrantHelpAcceptsManageAlone(t *testing.T) {
	var out, err strings.Builder
	code := Run([]string{"credentials", "grant", "--help"}, nil, &out, &err)
	if code != ExitSuccess || !strings.Contains(out.String(), "| --permission manage") || !strings.Contains(out.String(), "manage implies read/write") {
		t.Fatal(code, out.String(), err.String())
	}
}
