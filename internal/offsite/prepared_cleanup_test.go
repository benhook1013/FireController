package offsite

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Inspect the workspace at the actual transport boundary, while the session
// is still open and before remote verification creates its own expanded copy.
type preparedCheckingTransport struct {
	*memoryTransport
	t    *testing.T
	work string
	puts int
	gets int
}

func (m *preparedCheckingTransport) checkWorkspace() {
	m.t.Helper()
	err := filepath.WalkDir(m.work, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), "prepared-") || entry.Name() == "firecontroller.sqlite" {
			m.t.Errorf("local verification copy survived until remote work: %s", filepath.Base(name))
		}
		return nil
	})
	if err != nil {
		m.t.Fatal(err)
	}
}

func (m *preparedCheckingTransport) put(ctx context.Context, source, name string) error {
	m.checkWorkspace()
	m.puts++
	return m.memoryTransport.put(ctx, source, name)
}

func (m *preparedCheckingTransport) get(ctx context.Context, name, destination string) error {
	m.checkWorkspace()
	m.gets++
	return m.memoryTransport.get(ctx, name, destination)
}

func TestOffsitePreparedCopyRemovedBeforeUploadAndRecovery(t *testing.T) {
	f := newFixture(t)
	bundle := f.bundle(t)
	m := &preparedCheckingTransport{memoryTransport: memory(), t: t, work: filepath.Join(f.c.StateDir, "work")}
	m.uncertainReceipt = true
	_, err := push(t.Context(), f.c, m)
	wantCode(t, err, "offsite_transport_failed")
	st := state(t, f.c)
	if st.Pending == nil || st.Pending.ID != bundle.ID || st.LastSuccess != nil || m.puts != 2 || m.gets != 1 {
		t.Fatal("uncertain publication lost its original pending receipt", st, m.puts, m.gets)
	}
	pending := *st.Pending
	// Recovery must use the same archive and receipt even without the source.
	if err := os.RemoveAll(bundle.Path); err != nil {
		t.Fatal(err)
	}
	result, err := push(t.Context(), f.c, m)
	if err != nil || result.State != "complete" || result.BackupID != bundle.ID || m.gets != 2 {
		t.Fatal(result, err, m.gets)
	}
	st = state(t, f.c)
	if st.Pending != nil || st.LastSuccess == nil || *st.LastSuccess != pending || len(st.Owned) != 1 {
		t.Fatal("recovery changed the owned receipt", st)
	}
	archive := f.c.Remote + "/" + f.c.InstanceID + "/" + bundle.ID + "/archive.tar.gz"
	receipt := f.c.Remote + "/" + f.c.InstanceID + "/" + bundle.ID + "/receipt.json"
	if m.memoryTransport.puts[archive] != 1 || m.memoryTransport.puts[receipt] != 1 {
		t.Fatal("uncertain response caused duplicate publication", m.memoryTransport.puts)
	}
}
