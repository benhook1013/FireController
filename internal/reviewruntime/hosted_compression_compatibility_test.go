package reviewruntime

// Fixture-only compatibility investigation. No production encoding or paths change.
import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHostedCompressionRequiresExplicitReaderMigration(t *testing.T) {
	id, record := hostedCaptureFixture(t)
	store, root := newHostedTestStore(t)
	ref, err := store.Save(t.Context(), id, record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, hostedCaptureFilename(id, ref))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err = writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, compressed.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Read(t.Context(), id, ref); err == nil {
		t.Fatal("old reader silently accepted new physical format")
	}
	if err = os.Rename(path, path+".gz"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Read(t.Context(), id, ref); err == nil {
		t.Fatal("old reader silently followed alternate encoding")
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := io.ReadAll(io.LimitReader(reader, ref.Bytes+1))
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
	if !bytes.Equal(raw, restored) || captureReference(restored) != ref {
		t.Fatal("logical identity changed")
	}
	if err = os.WriteFile(path, restored, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := store.Read(t.Context(), id, ref)
	if err != nil || !reflect.DeepEqual(got, record) {
		t.Fatal("materialized raw capture changed user-visible observation", err)
	}
	t.Logf("synthetic canonical capture raw=%d gzip=%d; raw materialization retains exact IDs/hashes/records; no production codec", len(raw), compressed.Len())
}
