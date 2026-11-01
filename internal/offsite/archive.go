package offsite

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
)

var errTarSizeLimit = errors.New("offsite tar size limit")

// Count the expanded stream itself, including PAX records, padding and end
// markers, against the same limit enforced when unpacking it.
type tarSizeWriter struct {
	w         io.Writer
	remaining int64
}

func (w *tarSizeWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errTarSizeLimit
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func pack(ctx context.Context, source, destination string, c Config) (Receipt, error) {
	s, err := backup.Inspect(ctx, source)
	if err != nil {
		return Receipt{}, cancelOrFailure(ctx, "offsite_source_invalid")
	}
	if s.SourceInstanceID != c.InstanceID {
		return Receipt{}, failure("offsite_source_invalid")
	}
	f, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Receipt{}, failure("offsite_storage_unavailable")
	}
	complete := false
	defer func() {
		f.Close()
		if !complete {
			os.Remove(destination)
		}
	}()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(&tarSizeWriter{w: gz, remaining: c.MaxBundleBytes})
	count := 0
	err = filepath.WalkDir(source, func(file string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || ctx.Err() != nil {
			return cancelOrFailure(ctx, "offsite_source_invalid")
		}
		if d.IsDir() {
			return nil
		}
		s, err := d.Info()
		if err != nil || !s.Mode().IsRegular() || s.Mode()&os.ModeSymlink != 0 {
			return failure("offsite_source_invalid")
		}
		count++
		if count > 100003 || s.Size() < 0 || s.Size() > c.MaxBundleBytes {
			return errTarSizeLimit
		}
		rel, err := filepath.Rel(source, file)
		if err != nil {
			return failure("offsite_source_invalid")
		}
		if err = tw.WriteHeader(&tar.Header{Name: path.Join(filepath.Base(source), filepath.ToSlash(rel)), Mode: 0600, Size: s.Size(), ModTime: time.Unix(0, 0).UTC(), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			return err
		}
		in, err := os.Open(file)
		if err != nil {
			return err
		}
		defer in.Close()
		opened, err := in.Stat()
		if err != nil || !os.SameFile(s, opened) {
			return failure("offsite_source_invalid")
		}
		n, err := io.Copy(tw, &contextReader{ctx: ctx, r: io.LimitReader(in, s.Size()+1)})
		if err != nil {
			return err
		}
		if n != s.Size() {
			return failure("offsite_source_invalid")
		}
		return nil
	})
	if err != nil {
		tw.Close()
		gz.Close()
		if errors.Is(err, errTarSizeLimit) {
			return Receipt{}, failure("offsite_size_limit")
		}
		return Receipt{}, cancelOrFailure(ctx, "offsite_source_invalid")
	}
	if err = tw.Close(); err != nil {
		gz.Close()
		if errors.Is(err, errTarSizeLimit) {
			return Receipt{}, failure("offsite_size_limit")
		}
		return Receipt{}, failure("offsite_storage_unavailable")
	}
	if gz.Close() != nil {
		return Receipt{}, failure("offsite_storage_unavailable")
	}
	h, n, err := finishPackedArchive(ctx, f, c.MaxBundleBytes)
	if err != nil {
		return Receipt{}, err
	}
	r := Receipt{Version: 1, ID: s.ID, InstanceID: s.SourceInstanceID, SnapshotFinishedAt: s.SnapshotFinishedAt, EventSequence: s.EventSequence, ArchiveSHA256: h, ArchiveBytes: n}
	complete = true
	return r, nil
}

// Gzip framing can exceed the expanded-stream bound for incompressible input.
// Check the synced owned file before hashing treats an oversized input as bad.
func finishPackedArchive(ctx context.Context, f *os.File, limit int64) (string, int64, error) {
	syncErr := f.Sync()
	compressed, statErr := f.Stat()
	closeErr := f.Close()
	if syncErr != nil || statErr != nil || closeErr != nil {
		return "", 0, failure("offsite_storage_unavailable")
	}
	if ctx.Err() != nil {
		return "", 0, failure("offsite_cancelled")
	}
	if compressed.Size() > limit {
		return "", 0, failure("offsite_size_limit")
	}
	return fileHash(ctx, f.Name(), limit)
}

// A retained receipt remains historical ownership evidence when an operator
// lowers the transfer cap. Refuse work under the current budget without
// misreporting that valid history as a corrupt archive or pending publication.
func receiptTransferSize(ctx context.Context, r Receipt, c Config) error {
	if ctx.Err() != nil {
		return failure("offsite_cancelled")
	}
	if r.ArchiveBytes > c.MaxBundleBytes {
		return failure("offsite_size_limit")
	}
	return nil
}

func unpack(ctx context.Context, archive, destination string, r Receipt, c Config) (backup.Summary, error) {
	if err := receiptTransferSize(ctx, r, c); err != nil {
		return backup.Summary{}, err
	}
	f, h, n, err := openHashedArchive(ctx, archive, c.MaxBundleBytes)
	if err != nil {
		return backup.Summary{}, cancelOrFailure(ctx, "offsite_archive_invalid")
	}
	defer f.Close()
	if h != r.ArchiveSHA256 || n != r.ArchiveBytes {
		return backup.Summary{}, failure("offsite_archive_invalid")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return backup.Summary{}, failure("offsite_archive_invalid")
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return backup.Summary{}, failure("offsite_archive_invalid")
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: &contextReader{ctx: ctx, r: gz}, N: c.MaxBundleBytes + 1}
	tr := tar.NewReader(limited)
	count := 0
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return backup.Summary{}, cancelOrFailure(ctx, "offsite_archive_invalid")
		}
		count++
		if count > 100003 || hdr.Typeflag != tar.TypeReg || hdr.Size < 0 || hdr.Size > c.MaxBundleBytes || path.Clean(hdr.Name) != hdr.Name || strings.Contains(hdr.Name, "\\") || !strings.HasPrefix(hdr.Name, r.ID+"/") || seen[hdr.Name] {
			return backup.Summary{}, failure("offsite_archive_invalid")
		}
		seen[hdr.Name] = true
		file := filepath.Join(destination, filepath.FromSlash(hdr.Name))
		if err = os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			return backup.Summary{}, failure("offsite_storage_unavailable")
		}
		out, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return backup.Summary{}, failure("offsite_archive_invalid")
		}
		n, copyErr := io.Copy(out, tr)
		syncErr := out.Sync()
		closeErr := out.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil || n != hdr.Size {
			return backup.Summary{}, cancelOrFailure(ctx, "offsite_archive_invalid")
		}
	}
	// Consume the gzip trailer so corruption after the tar end is not ignored.
	if _, err = io.Copy(io.Discard, limited); err != nil || limited.N <= 0 {
		return backup.Summary{}, cancelOrFailure(ctx, "offsite_archive_invalid")
	}
	s, err := backup.Inspect(ctx, filepath.Join(destination, r.ID))
	if err != nil {
		return backup.Summary{}, cancelOrFailure(ctx, "offsite_archive_invalid")
	}
	if s.SourceInstanceID != r.InstanceID || s.SnapshotFinishedAt != r.SnapshotFinishedAt || s.EventSequence != r.EventSequence {
		return backup.Summary{}, failure("offsite_archive_invalid")
	}
	// Persist directory entries from leaves to root before a fetched bundle is
	// reported complete. Restore still performs its separate inactive staging.
	var dirs []string
	err = filepath.WalkDir(destination, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return backup.Summary{}, failure("offsite_storage_unavailable")
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err = syncDir(dirs[i]); err != nil {
			return backup.Summary{}, err
		}
	}
	return s, nil
}
