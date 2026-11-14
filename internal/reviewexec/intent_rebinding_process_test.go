package reviewexec

import (
	"bytes"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestIntentRebindProcessHelper(t *testing.T) {
	if os.Getenv("FC_INTENT_TEST_HELPER") != "1" {
		return
	}
	store, err := core.Open(t.Context(), os.Getenv("FC_INTENT_TEST_STORE"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err := New(store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	old, err := readOperation(t.Context(), store.DB(), attemptProject, os.Getenv("FC_INTENT_TEST_OPERATION"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(os.Getenv("FC_INTENT_TEST_READY"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err = os.Stat(os.Getenv("FC_INTENT_TEST_GO")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bounded process barrier expired")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for tries := 0; tries < 50; tries++ {
		_, err = engine.rebindQueuedIntent(t.Context(), old, freshIntentSource(old))
		if err == nil {
			return
		}
		var refusal *core.Error
		if errors.As(err, &refusal) && refusal.Code == "review_conflict" {
			os.Exit(3)
		}
		if !errors.As(err, &refusal) || refusal.Code != "overloaded" {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("bounded local contention did not settle", err)
}

func TestQueuedIntentRebindCrossProcessSingleWinnerAndRestart(t *testing.T) {
	f := supervisorFixture(t)
	r := f.enqueue(t, attemptProject, "cross-process-intent", "cli", 2, 1)
	var seq int
	var name, dbpath string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &dbpath); err != nil {
		t.Fatal(err)
	}
	barrier := t.TempDir()
	goFile := filepath.Join(barrier, "go")
	commands := make([]*exec.Cmd, 2)
	outputs := make([]bytes.Buffer, 2)
	for i := range 2 {
		ready := filepath.Join(barrier, string(rune('a'+i)))
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestIntentRebindProcessHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "FC_INTENT_TEST_HELPER=1", "FC_INTENT_TEST_STORE="+filepath.Dir(dbpath), "FC_INTENT_TEST_OPERATION="+r.ID, "FC_INTENT_TEST_READY="+ready, "FC_INTENT_TEST_GO="+goFile)
		cmd.Stdout = &outputs[i]
		cmd.Stderr = &outputs[i]
		commands[i] = cmd
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, a := os.Stat(filepath.Join(barrier, "a"))
		_, b := os.Stat(filepath.Join(barrier, "b"))
		if a == nil && b == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("children did not reach replacement barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(goFile, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	won, lost := 0, 0
	for i, cmd := range commands {
		err := cmd.Wait()
		if err == nil {
			won++
		} else {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 3 {
				lost++
			} else {
				t.Fatalf("child failed: %v %s", err, outputs[i].String())
			}
		}
	}
	if won != 1 || lost != 1 {
		t.Fatal(won, lost)
	}
	var next string
	if err := f.store.DB().QueryRow(`SELECT successor_operation_id FROM review_intent_rebindings WHERE predecessor_operation_id=?`, r.ID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	cliAllowanceReopen(t, f)
	var err error
	f.e, err = New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	current := cliWaitRead(t, f, next)
	if current.State != "queued" || current.Intent == nil || current.Intent.RootOperationID != r.ID {
		t.Fatal(current)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 2 || roundRead(t, f, 2, "cli").Used != 0 {
		t.Fatal("restart duplicated or charged intent")
	}
}
