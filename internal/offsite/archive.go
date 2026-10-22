package offsite

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
)

func pack(ctx context.Context, source, destination string, c Config) (Receipt, error) {
	s, err := backup.Inspect(ctx, source)
	if err != nil {
		return Receipt{}, failure("offsite_source_invalid")
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
	tw := tar.NewWriter(gz)
	var total int64
	count := 0
	err = filepath.WalkDir(source, func(file string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || ctx.Err() != nil {
			return failure("offsite_source_invalid")
		}
		if d.IsDir() {
			return nil
		}
		s, err := d.Info()
		if err != nil || !s.Mode().IsRegular() || s.Mode()&os.ModeSymlink != 0 {
			return failure("offsite_source_invalid")
		}
		count++
		if count > 100003 || s.Size() < 0 || s.Size() > c.MaxBundleBytes-total-512 {
			return failure("offsite_size_limit")
		}
		total += s.Size() + 512
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
		if err != nil || n != s.Size() {
			return failure("offsite_source_invalid")
		}
		return nil
	})
	if err != nil {
		tw.Close()
		gz.Close()
		return Receipt{}, failure("offsite_source_invalid")
	}
	if tw.Close() != nil || gz.Close() != nil || f.Sync() != nil || f.Close() != nil {
		return Receipt{}, failure("offsite_storage_unavailable")
	}
	h, n, err := fileHash(ctx, destination, c.MaxBundleBytes)
	if err != nil {
		return Receipt{}, err
	}
	r := Receipt{Version: 1, ID: s.ID, InstanceID: s.SourceInstanceID, SnapshotFinishedAt: s.SnapshotFinishedAt, EventSequence: s.EventSequence, ArchiveSHA256: h, ArchiveBytes: n}
	complete = true
	return r, nil
}

func unpack(ctx context.Context, archive, destination string, r Receipt, c Config) (backup.Summary, error) {
	h, n, err := fileHash(ctx, archive, c.MaxBundleBytes)
	if err != nil || h != r.ArchiveSHA256 || n != r.ArchiveBytes {
		return backup.Summary{}, failure("offsite_archive_invalid")
	}
	f, err := os.Open(archive)
	if err != nil {
		return backup.Summary{}, failure("offsite_archive_invalid")
	}
	defer f.Close()
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
			return backup.Summary{}, failure("offsite_archive_invalid")
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
			return backup.Summary{}, failure("offsite_archive_invalid")
		}
	}
	// Consume the gzip trailer so corruption after the tar end is not ignored.
	if _, err = io.Copy(io.Discard, limited); err != nil || limited.N <= 0 {
		return backup.Summary{}, failure("offsite_archive_invalid")
	}
	s, err := backup.Inspect(ctx, filepath.Join(destination, r.ID))
	if err != nil || s.SourceInstanceID != r.InstanceID || s.SnapshotFinishedAt != r.SnapshotFinishedAt || s.EventSequence != r.EventSequence {
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
