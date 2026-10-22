package offsite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/benhook1013/FireController/internal/backup"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type Result struct {
	State            string `json:"state"`
	BackupID         string `json:"backup_id"`
	SourceInstanceID string `json:"source_instance_id"`
	Retained         int    `json:"retained"`
}

type session struct {
	c         Config
	state     Status
	transport transport
	lockFile  *os.File
	work      string
}

func openSession(c Config, t transport) (*session, error) {
	if err := privateDir(c.StateDir, true); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(c.StateDir, "transfer.lock")
	if info, err := os.Lstat(lockPath); err == nil && !info.Mode().IsRegular() {
		return nil, failure("offsite_state_invalid")
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, failure("offsite_storage_unavailable")
	}
	if lock(f) != nil {
		f.Close()
		return nil, failure("offsite_busy")
	}
	s := &session{c: c, lockFile: f}
	s.state, err = readState(c)
	if err != nil {
		s.close()
		return nil, err
	}
	s.work, err = prepareWork(c)
	if err != nil {
		s.close()
		return nil, failure("offsite_storage_unavailable")
	}
	if t == nil {
		t, err = newTransport(c, s.work)
	}
	if err != nil {
		s.close()
		return nil, err
	}
	s.transport = t
	return s, nil
}

// A single reserved workspace bounds disk use across hard kills. Only a
// directory carrying this connection's marker may be reclaimed after a crash.
func prepareWork(c Config) (string, error) {
	dir := filepath.Join(c.StateDir, "work")
	if _, err := os.Lstat(dir); err == nil {
		if err = privateDir(dir, false); err != nil {
			return "", err
		}
		b, err := privateRead(filepath.Join(dir, ".owner"), 128)
		if err != nil || string(b) != binding(c) {
			// A kill between mkdir and the ownership write can leave an empty
			// directory. Remove handles only that empty case, never its contents.
			if os.Remove(dir) != nil {
				return "", failure("offsite_state_invalid")
			}
		} else if os.RemoveAll(dir) != nil {
			return "", failure("offsite_storage_unavailable")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", failure("offsite_storage_unavailable")
	}
	if os.Mkdir(dir, 0700) != nil {
		return "", failure("offsite_storage_unavailable")
	}
	marker := filepath.Join(dir, ".owner")
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", failure("offsite_storage_unavailable")
	}
	_, writeErr := f.Write([]byte(binding(c)))
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || syncDir(dir) != nil {
		return "", failure("offsite_storage_unavailable")
	}
	return dir, nil
}

func (s *session) close() {
	if s.work != "" {
		os.RemoveAll(s.work)
	}
	if s.lockFile != nil {
		s.lockFile.Close()
	}
}
func (s *session) save() error {
	return writeJSON(filepath.Join(s.c.StateDir, "offsite.json"), s.state)
}
func (s *session) object(r Receipt, file string) string {
	return s.c.Remote + "/" + s.c.InstanceID + "/" + r.ID + "/" + file
}

func (s *session) remoteReceipt(ctx context.Context, r Receipt) (Receipt, error) {
	value, err := s.receiptAt(ctx, s.object(r, "receipt.json"))
	if err == nil && value.ID != r.ID {
		return value, failure("offsite_remote_conflict")
	}
	return value, err
}

func (s *session) receiptAt(ctx context.Context, name string) (Receipt, error) {
	b, err := s.transport.read(ctx, name)
	if err != nil {
		return Receipt{}, err
	}
	var value Receipt
	if len(b) > 4096 || strictjson.Decode(b, &value) != nil || !validReceipt(value, s.c) {
		return value, failure("offsite_remote_conflict")
	}
	return value, nil
}

func (s *session) publishLatest(ctx context.Context, r Receipt) error {
	name := s.c.Remote + "/" + s.c.InstanceID + "/latest.json"
	old, err := s.receiptAt(ctx, name)
	if err != nil && !errors.Is(err, notFound) {
		return err
	}
	if err == nil && old == r {
		return nil
	}
	if err == nil {
		at, _ := time.Parse(time.RFC3339Nano, old.SnapshotFinishedAt)
		next, _ := time.Parse(time.RFC3339Nano, r.SnapshotFinishedAt)
		if old.EventSequence > r.EventSequence || old.EventSequence == r.EventSequence && at.After(next) {
			return failure("offsite_source_rollback")
		}
	}
	file := filepath.Join(s.work, "latest.json")
	if err = writeJSON(file, r); err != nil {
		return err
	}
	if err = s.transport.put(ctx, file, name); err != nil {
		return err
	}
	actual, err := s.receiptAt(ctx, name)
	if err != nil {
		return err
	}
	if actual != r {
		return failure("offsite_remote_conflict")
	}
	return nil
}

func (s *session) verifyRemote(ctx context.Context, r Receipt) error {
	download := filepath.Join(s.work, "verify.tar.gz")
	if err := s.transport.get(ctx, s.object(r, "archive.tar.gz"), download); err != nil {
		return err
	}
	defer os.Remove(download)
	stage, err := os.MkdirTemp(s.work, "verify-")
	if err != nil {
		return failure("offsite_storage_unavailable")
	}
	defer os.RemoveAll(stage)
	_, err = unpack(ctx, download, stage, r, s.c)
	return err
}

func newest(ctx context.Context, c Config) (backup.Summary, error) {
	if err := privateDir(c.SourceRoot, false); err != nil {
		return backup.Summary{}, err
	}
	f, err := os.Open(c.SourceRoot)
	if err != nil {
		return backup.Summary{}, failure("offsite_source_invalid")
	}
	defer f.Close()
	entries, err := f.ReadDir(1001)
	if err != nil {
		return backup.Summary{}, failure("offsite_source_invalid")
	}
	if len(entries) > 1000 {
		return backup.Summary{}, failure("offsite_source_limit")
	}
	var selected backup.Manifest
	for _, entry := range entries {
		if !bundleID.MatchString(entry.Name()) {
			continue
		}
		if ctx.Err() != nil || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return backup.Summary{}, failure("offsite_source_invalid")
		}
		b, err := privateRead(filepath.Join(c.SourceRoot, entry.Name(), "manifest.json"), 64<<20)
		if err != nil {
			return backup.Summary{}, failure("offsite_source_invalid")
		}
		var m backup.Manifest
		if strictjson.Decode(b, &m) != nil || m.ID != entry.Name() {
			return backup.Summary{}, failure("offsite_source_invalid")
		}
		if m.SourceInstanceID != c.InstanceID {
			continue
		}
		at, e := time.Parse(time.RFC3339Nano, m.SnapshotFinishedAt)
		if e != nil {
			return backup.Summary{}, failure("offsite_source_invalid")
		}
		previous, _ := time.Parse(time.RFC3339Nano, selected.SnapshotFinishedAt)
		if selected.ID == "" || m.EventSequence > selected.EventSequence || m.EventSequence == selected.EventSequence && (at.After(previous) || at.Equal(previous) && m.ID > selected.ID) {
			selected = m
		}
	}
	if selected.ID == "" {
		return backup.Summary{}, failure("offsite_source_empty")
	}
	s, err := backup.Inspect(ctx, filepath.Join(c.SourceRoot, selected.ID))
	if err != nil || s.SourceInstanceID != c.InstanceID {
		return s, failure("offsite_source_invalid")
	}
	return s, nil
}

// Push performs one finite operator cycle. A deployment timer owns cadence;
// no serving process, per-agent read or provider review path calls it.
func Push(ctx context.Context, configPath string) (Result, error) {
	c, err := loadConfig(configPath)
	if err != nil {
		return Result{}, err
	}
	return push(ctx, c, nil)
}

func push(ctx context.Context, c Config, t transport) (result Result, err error) {
	if ctx == nil || ctx.Err() != nil {
		return result, failure("offsite_cancelled")
	}
	duration, _ := time.ParseDuration(c.Timeout)
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	s, err := openSession(c, t)
	if err != nil {
		return result, err
	}
	defer s.close()
	s.state.LastAttempt = &Attempt{State: "running", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = s.save(); err != nil {
		return result, err
	}
	defer func() {
		s.state.LastAttempt.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err != nil {
			s.state.LastAttempt.State = "failed"
			s.state.LastAttempt.Reason = "offsite_failed"
			var e *core.Error
			if errors.As(err, &e) {
				s.state.LastAttempt.Reason = e.Code
			}
		} else {
			s.state.LastAttempt.State = result.State
		}
		if saveErr := s.save(); saveErr != nil {
			err = saveErr
		}
	}()
	archive := filepath.Join(c.StateDir, "pending.tar.gz")
	if s.state.Pending == nil {
		// A crash after committing success but before deleting its spool leaves
		// only this exact, verifiable last-success archive eligible for cleanup.
		if _, e := os.Lstat(archive); e == nil {
			if s.state.LastSuccess == nil {
				return result, failure("offsite_state_invalid")
			}
			h, n, e := fileHash(ctx, archive, c.MaxBundleBytes)
			if e != nil || h != s.state.LastSuccess.ArchiveSHA256 || n != s.state.LastSuccess.ArchiveBytes {
				return result, failure("offsite_state_invalid")
			}
			if os.Remove(archive) != nil {
				return result, failure("offsite_storage_unavailable")
			}
		}
		// Resolve unfinished retention before allowing another upload to expand
		// the owned inventory. LastSuccess remains independently reportable.
		if len(s.state.Owned) > c.Keep {
			if err = s.prune(ctx, false); err != nil {
				return result, err
			}
		}
		selected, e := newest(ctx, c)
		if e != nil {
			return result, e
		}
		for _, r := range s.state.Owned {
			if selected.ID == r.ID {
				return Result{State: "unchanged", BackupID: selected.ID, SourceInstanceID: c.InstanceID, Retained: len(s.state.Owned)}, nil
			}
		}
		if s.state.LastSuccess != nil && selected.EventSequence < s.state.LastSuccess.EventSequence {
			return result, failure("offsite_source_rollback")
		}
		if len(s.state.Owned) >= maxOwned {
			return result, failure("offsite_retention_limit")
		}
		// Preparation uses a stable, separately owned path. No remote effect is
		// possible before its receipt and complete archive are durable locally.
		prepared := filepath.Join(c.StateDir, "prepare.tar.gz")
		if info, e := os.Lstat(prepared); e == nil {
			if !info.Mode().IsRegular() {
				return result, failure("offsite_state_invalid")
			}
			if os.Remove(prepared) != nil {
				return result, failure("offsite_storage_unavailable")
			}
		}
		r, e := pack(ctx, selected.Path, prepared, c)
		if e != nil {
			return result, e
		}
		// Inspect the captured archive, not the source again: local retention
		// can remove the original after it was selected.
		check, e := os.MkdirTemp(s.work, "prepared-")
		if e != nil {
			return result, failure("offsite_storage_unavailable")
		}
		if _, e = unpack(ctx, prepared, check, r, c); e != nil {
			return result, e
		}
		// Persist the intent before moving the complete spool. Recovery can
		// finish either side of that rename without needing the source bundle.
		s.state.Pending = &r
		if err = s.save(); err != nil {
			return result, err
		}
	}
	r := *s.state.Pending
	if _, e := os.Lstat(archive); errors.Is(e, os.ErrNotExist) {
		prepared := filepath.Join(c.StateDir, "prepare.tar.gz")
		h, n, e := fileHash(ctx, prepared, c.MaxBundleBytes)
		if e != nil || h != r.ArchiveSHA256 || n != r.ArchiveBytes {
			return result, failure("offsite_pending_invalid")
		}
		if os.Rename(prepared, archive) != nil || syncDir(c.StateDir) != nil {
			return result, failure("offsite_storage_unavailable")
		}
	}
	h, n, e := fileHash(ctx, archive, c.MaxBundleBytes)
	if e != nil || h != r.ArchiveSHA256 || n != r.ArchiveBytes {
		return result, failure("offsite_pending_invalid")
	}
	remote, e := s.remoteReceipt(ctx, r)
	if e != nil && !errors.Is(e, notFound) {
		return result, e
	}
	if e == nil && remote != r {
		return result, failure("offsite_remote_conflict")
	}
	if errors.Is(e, notFound) {
		if err = s.transport.put(ctx, archive, s.object(r, "archive.tar.gz")); err != nil {
			return result, err
		}
	}
	if err = s.verifyRemote(ctx, r); err != nil {
		return result, err
	}
	if errors.Is(e, notFound) {
		receiptPath := filepath.Join(s.work, "receipt.json")
		if err = writeJSON(receiptPath, r); err != nil {
			return result, err
		}
		if err = s.transport.put(ctx, receiptPath, s.object(r, "receipt.json")); err != nil {
			return result, err
		}
	}
	remote, e = s.remoteReceipt(ctx, r)
	if e != nil || remote != r {
		if e != nil {
			return result, e
		}
		return result, failure("offsite_remote_conflict")
	}
	if err = s.publishLatest(ctx, r); err != nil {
		return result, err
	}
	s.state.LastSuccess = &r
	s.state.Pending = nil
	s.state.Owned = append(s.state.Owned, r)
	if err = s.save(); err != nil {
		return result, err
	}
	if os.Remove(archive) != nil || syncDir(c.StateDir) != nil {
		return result, failure("offsite_storage_unavailable")
	}
	if err = s.prune(ctx, true); err != nil {
		return result, err
	}
	return Result{State: "complete", BackupID: r.ID, SourceInstanceID: c.InstanceID, Retained: len(s.state.Owned)}, nil
}

func (s *session) prune(ctx context.Context, newestVerified bool) (err error) {
	if len(s.state.Owned) <= s.c.Keep {
		s.state.RetentionReason = ""
		return nil
	}
	sort.Slice(s.state.Owned, func(i, j int) bool {
		a, b := s.state.Owned[i], s.state.Owned[j]
		if a.EventSequence != b.EventSequence {
			return a.EventSequence > b.EventSequence
		}
		at, _ := time.Parse(time.RFC3339Nano, a.SnapshotFinishedAt)
		bt, _ := time.Parse(time.RFC3339Nano, b.SnapshotFinishedAt)
		if at.Equal(bt) {
			return a.ID > b.ID
		}
		return at.After(bt)
	})
	if s.state.LastSuccess == nil || s.state.LastSuccess.ID != s.state.Owned[0].ID {
		return failure("offsite_retention_conflict")
	}
	if !newestVerified {
		r := *s.state.LastSuccess
		actual, e := s.remoteReceipt(ctx, r)
		if e != nil || actual != r {
			return failure("offsite_retention_conflict")
		}
		if err = s.verifyRemote(ctx, r); err != nil {
			return err
		}
	}
	defer func() {
		if err != nil {
			s.state.RetentionReason = "offsite_retention_failed"
		} else {
			s.state.RetentionReason = ""
		}
	}()
	for len(s.state.Owned) > s.c.Keep {
		index := len(s.state.Owned) - 1
		r := s.state.Owned[index]
		actual, e := s.remoteReceipt(ctx, r)
		if e != nil && !errors.Is(e, notFound) {
			return e
		}
		if e == nil && actual != r {
			return failure("offsite_remote_conflict")
		}
		// Matching ownership alone is insufficient if an archive was replaced.
		archive := filepath.Join(s.work, "prune.tar.gz")
		e = s.transport.get(ctx, s.object(r, "archive.tar.gz"), archive)
		if e != nil && !errors.Is(e, notFound) {
			return e
		}
		if e == nil {
			h, n, e := fileHash(ctx, archive, s.c.MaxBundleBytes)
			os.Remove(archive)
			if e != nil || h != r.ArchiveSHA256 || n != r.ArchiveBytes {
				return failure("offsite_remote_conflict")
			}
		}
		if err = s.transport.remove(ctx, s.object(r, "archive.tar.gz")); err != nil {
			return err
		}
		if err = s.transport.remove(ctx, s.object(r, "receipt.json")); err != nil {
			return err
		}
		s.state.Owned = s.state.Owned[:index]
		if err = s.save(); err != nil {
			return err
		}
	}
	return nil
}

func Fetch(ctx context.Context, configPath, id, destination string) (backup.Summary, error) {
	c, err := loadConfig(configPath)
	if err != nil {
		return backup.Summary{}, err
	}
	if ctx == nil || id != "" && !bundleID.MatchString(id) || !absolute(destination) {
		return backup.Summary{}, failure("offsite_configuration_invalid")
	}
	duration, _ := time.ParseDuration(c.Timeout)
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	s, err := openSession(c, nil)
	if err != nil {
		return backup.Summary{}, err
	}
	defer s.close()
	return s.fetch(ctx, id, destination)
}

func (s *session) fetch(ctx context.Context, id, destination string) (result backup.Summary, err error) {
	var latest *Receipt
	if id == "" {
		r, e := s.receiptAt(ctx, s.c.Remote+"/"+s.c.InstanceID+"/latest.json")
		if e != nil {
			return result, e
		}
		latest = &r
		id = r.ID
	}
	r, err := s.remoteReceipt(ctx, Receipt{ID: id})
	if err != nil {
		return result, err
	}
	if latest != nil && *latest != r {
		return result, failure("offsite_remote_conflict")
	}
	if err = os.Mkdir(destination, 0700); err != nil {
		return result, failure("offsite_destination_exists")
	}
	complete := false
	defer func() {
		if !complete {
			os.RemoveAll(destination)
		}
	}()
	archive := filepath.Join(s.work, "fetch.tar.gz")
	if err = s.transport.get(ctx, s.object(r, "archive.tar.gz"), archive); err != nil {
		return result, err
	}
	if result, err = unpack(ctx, archive, destination, r, s.c); err != nil {
		return result, err
	}
	if err = syncDir(destination); err != nil {
		return result, err
	}
	complete = true
	return result, nil
}
