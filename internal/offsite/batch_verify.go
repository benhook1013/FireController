package offsite

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"path"
	"path/filepath"
	"strings"
)

func (p *batchPreparation) admit(dir string, additional int64) error {
	return batchHeadroom(dir, p.floor, additional)
}

// Trust is process-local and exact-receipt only. Every destination still
// downloads and hashes its own bytes. Equal bytes need not repeat extraction
// and database inspection that already succeeded in this same batch.
func (p *batchPreparation) validateArchive(ctx context.Context, archive, stage string, r Receipt, c Config) error {
	need, err := archiveExpansionBudget(ctx, archive, r, c)
	if err != nil {
		return err
	}
	if err = p.admit(filepath.Dir(stage), need); err != nil {
		return err
	}
	if _, err = unpack(ctx, archive, stage, r, c); err != nil {
		return err
	}
	if p.verified == nil {
		p.verified = make(map[Receipt]bool)
	}
	p.verified[r] = true
	return nil
}

// Measure the hash-bound archive before creating its expanded files. Budget
// regular files at conservative 64KiB allocation units and directory metadata
// separately; the bounded gzip stream remains subject to MaxBundleBytes.
// This is not a universal filesystem quota: the configured reserve also covers
// filesystem metadata and unrelated writers, as for every prospective check.
func archiveExpansionBudget(ctx context.Context, archive string, r Receipt, c Config) (int64, error) {
	f, h, n, err := openHashedArchive(ctx, archive, r.ArchiveBytes)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if h != r.ArchiveSHA256 || n != r.ArchiveBytes {
		return 0, failure("offsite_archive_invalid")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return 0, failure("offsite_storage_unavailable")
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, failure("offsite_archive_invalid")
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: &contextReader{ctx: ctx, r: gz}, N: c.MaxBundleBytes + 1}
	tr := tar.NewReader(limited)
	const unit int64 = 64 << 10
	const metadataMargin int64 = 16 << 20
	var bytes int64
	count := 0
	dirs := map[string]bool{}
	seen := map[string]bool{}
	for {
		hdr, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return 0, cancelOrFailure(ctx, "offsite_archive_invalid")
		}
		count++
		if count > 100003 || hdr.Typeflag != tar.TypeReg || hdr.Size < 0 || hdr.Size > c.MaxBundleBytes || path.Clean(hdr.Name) != hdr.Name || strings.Contains(hdr.Name, "\\") || !strings.HasPrefix(hdr.Name, r.ID+"/") || seen[hdr.Name] {
			return 0, failure("offsite_archive_invalid")
		}
		seen[hdr.Name] = true
		bytes += ((hdr.Size + unit - 1) / unit) * unit
		for d := path.Dir(hdr.Name); d != "."; d = path.Dir(d) {
			dirs[d] = true
			if len(dirs) > 800024 {
				return 0, failure("offsite_size_limit")
			}
		}
		if _, e = io.Copy(io.Discard, tr); e != nil {
			return 0, cancelOrFailure(ctx, "offsite_archive_invalid")
		}
	}
	if _, err = io.Copy(io.Discard, limited); err != nil || limited.N <= 0 {
		return 0, cancelOrFailure(ctx, "offsite_archive_invalid")
	}
	return bytes + int64(len(dirs))*unit + metadataMargin, nil
}
