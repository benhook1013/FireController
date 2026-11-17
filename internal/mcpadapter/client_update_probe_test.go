package mcpadapter

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/buildinfo"
)

func waitClientProbeIdle(t *testing.T, probe *clientUpdateProbe) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		probe.mu.Lock()
		inflight := probe.inflight
		probe.mu.Unlock()
		if !inflight {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("released metadata probe did not finish")
}

func TestClientUpdateProbeNeverDelaysReceiptOrAccumulatesBlockedReads(t *testing.T) {
	type answer struct {
		source    buildinfo.Source
		available bool
	}
	answers, stop := make(chan answer), make(chan struct{})
	entered := make(chan struct{}, 4)
	var reads atomic.Int32
	probe := &clientUpdateProbe{read: func() (buildinfo.Source, bool) {
		reads.Add(1)
		entered <- struct{}{}
		select {
		case value := <-answers:
			return value.source, value.available
		case <-stop:
			return buildinfo.Source{}, false
		}
	}}
	n := &clientUpdateNotice{running: buildinfo.Client{SourceRevision: strings.Repeat("a", 40), SourceState: "clean"}, target: probe.snapshot}
	t.Cleanup(func() { close(stop); waitClientProbeIdle(t, probe) })
	unchanged := func() {
		t.Helper()
		r := updateResult(map[string]any{"id": "original-receipt", "revision": 2, "state": "finalized"})
		before, _ := json.Marshal(r)
		done := make(chan struct{})
		go func() { n.Apply(r); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("optional metadata blocked completed receipt")
		}
		after, _ := json.Marshal(r)
		if string(before) != string(after) {
			t.Fatal("blocked or unchanged target modified original receipt", string(after))
		}
	}
	unchanged()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("demand did not start metadata probe")
	}
	for range 32 {
		unchanged()
	}
	if reads.Load() != 1 {
		t.Fatal("blocked read spawned per-result probes", reads.Load())
	}
	for i, revision := range []string{strings.Repeat("b", 40), strings.Repeat("c", 40)} {
		answers <- answer{buildinfo.Source{Revision: revision, State: "clean"}, true}
		waitClientProbeIdle(t, probe)
		r := updateResult(map[string]any{"id": "original-receipt", "revision": 2, "state": "finalized"})
		n.Apply(r)
		body, _ := json.Marshal(r.StructuredContent)
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil || fields["client_update"] == nil || !strings.Contains(string(fields["client_update"]), revision) || string(fields["revision"]) != "2" || string(fields["state"]) != `"finalized"` {
			t.Fatal("completed target did not annotate next original receipt", string(body))
		}
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("next demand did not refresh metadata")
		}
		for range 8 {
			unchanged()
		}
		if reads.Load() != int32(i+2) {
			t.Fatal("cache duplicated background probes", reads.Load())
		}
	}
	// Unavailable target metadata omits advisory output without changing the
	// receipt; the caller need not wait for it or treat it as an operation error.
	answers <- answer{}
	waitClientProbeIdle(t, probe)
	unchanged()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("final demand did not start")
	}
}

func TestClientUpdateNoticeRealConstructorProbesOffResponsePath(t *testing.T) {
	release, entered, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	n := newClientUpdateNoticeWithProbe(func() (buildinfo.Source, bool) {
		close(entered)
		<-release
		close(finished)
		return buildinfo.Source{}, false
	})
	n.running = buildinfo.Client{SourceRevision: strings.Repeat("a", 40), SourceState: "clean"}
	t.Cleanup(func() {
		close(release)
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("constructor probe did not finish")
		}
	})
	r := updateResult(map[string]string{"state": "finalized"})
	before, _ := json.Marshal(r)
	done := make(chan struct{})
	go func() { n.Apply(r); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("real constructor waited for filesystem metadata")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("real constructor did not use demand probe")
	}
	after, _ := json.Marshal(r)
	if string(before) != string(after) {
		t.Fatal("unavailable asynchronous metadata changed receipt")
	}
}
