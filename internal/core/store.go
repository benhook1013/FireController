package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const schemaVersion = 1

type Store struct {
	db               *sql.DB
	writer           *sql.DB
	admission        chan struct{}
	instanceID       string
	signingKey       []byte
	eventMu          sync.Mutex
	eventGroups      map[messageTopic]map[*messageWaiter]struct{}
	eventCount       int
	principalWaiters map[string]int
}

func (s *Store) InstanceID() string { return s.instanceID }
func (s *Store) DB() *sql.DB        { return s.db }
func (s *Store) Close() error       { return errors.Join(s.db.Close(), s.writer.Close()) }

func Open(ctx context.Context, dataDir string) (*Store, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, Fail("configuration_invalid", "A private data directory is required.", 500)
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, Fail("storage_unavailable", "The private data directory could not be created.", 500)
	}
	info, err := os.Lstat(dataDir)
	if err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return nil, Fail("configuration_invalid", "Use a private directory with access restricted to its owner.", 500)
	}
	path := filepath.Join(dataDir, "firecontroller.sqlite")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		err = f.Close()
	} else if errors.Is(err, os.ErrExist) {
		info, err = os.Lstat(path)
		if err == nil && (!info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0)) {
			err = errors.New("unsafe database file")
		}
	}
	if err != nil {
		return nil, Fail("storage_unavailable", "The private database file is unavailable or unsafe.", 500)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, Fail("storage_unavailable", "The database location is invalid.", 500)
	}
	u := databaseURL(absolute)
	q := url.Values{"_pragma": {"foreign_keys(1)", "synchronous(FULL)", "busy_timeout(0)"}, "_txlock": {"immediate"}}
	u.RawQuery = q.Encode()
	writer, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, Fail("storage_unavailable", "The database could not be opened.", 500)
	}
	writer.SetMaxOpenConns(1)
	s := &Store{writer: writer, admission: make(chan struct{}, 1)}
	if err = s.initialize(ctx, dataDir); err != nil {
		_ = writer.Close()
		return nil, err
	}
	q.Del("_txlock")
	u.RawQuery = q.Encode()
	s.db, err = sql.Open("sqlite", u.String())
	if err != nil {
		_ = writer.Close()
		return nil, Fail("storage_unavailable", "The database could not be opened.", 500)
	}
	s.db.SetMaxOpenConns(4)
	if err = s.db.PingContext(ctx); err != nil {
		_ = s.Close()
		return nil, storageError(err)
	}
	return s, nil
}

// A Windows drive path needs a leading slash in a file URI. Without it,
// net/url serializes C:/... as the authority in file://C:/..., which SQLite
// rejects. Keep filename punctuation escaped as path data on every platform.
func databaseURL(absolute string) url.URL {
	path := filepath.ToSlash(absolute)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return url.URL{Scheme: "file", Path: path}
}

func (s *Store) initialize(ctx context.Context, dir string) error {
	conn, err := s.writer.Conn(ctx)
	if err != nil {
		return storageError(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		return storageError(err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var version int
	if err = conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return storageError(err)
	}
	if version > schemaVersion {
		return Fail("schema_unsupported", "The database schema is newer than this service; use a compatible binary.", 500)
	}
	if version == 0 {
		// A fresh schema must not adopt an unrelated unversioned database.
		var tables int
		if err = conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
			return storageError(err)
		}
		if tables != 0 {
			return Fail("schema_unsupported", "The unversioned database is not a fresh FireController database.", 500)
		}
		token, err := bootstrapToken(dir)
		if err != nil {
			return err
		}
		key := make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return Fail("bootstrap_failed", "Secure randomness is unavailable.", 500)
		}
		for _, statement := range schema {
			if _, err = conn.ExecContext(ctx, statement); err != nil {
				return storageError(err)
			}
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES('instance_id',?),('signing_key',?)", NewID("instance"), hex.EncodeToString(key)); err != nil {
			return storageError(err)
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO credentials(id,principal_id,token_sha256,is_admin,revoked) VALUES(?,?,?,1,0)", NewID("credential"), NewID("principal"), tokenHash(token)); err != nil {
			return storageError(err)
		}
		if _, err = conn.ExecContext(ctx, "PRAGMA user_version=1"); err != nil {
			return storageError(err)
		}
	}
	var key string
	if err = conn.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='instance_id'").Scan(&s.instanceID); err != nil {
		return Fail("schema_invalid", "Required instance metadata is unavailable.", 500)
	}
	if err = conn.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='signing_key'").Scan(&key); err != nil {
		return Fail("schema_invalid", "Required signing metadata is unavailable.", 500)
	}
	s.signingKey, err = hex.DecodeString(key)
	if err != nil || len(s.signingKey) != 32 || s.instanceID == "" {
		return Fail("schema_invalid", "Required instance metadata is invalid.", 500)
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return storageError(err)
	}
	committed = true
	var journal string
	if err = conn.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journal); err != nil || journal != "wal" {
		return Fail("storage_unavailable", "The database cannot enable durable WAL storage.", 500)
	}
	return nil
}

func bootstrapToken(dir string) (string, error) {
	path := filepath.Join(dir, "bootstrap-admin.token")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		info, e := os.Lstat(path)
		if e != nil || !info.Mode().IsRegular() || info.Size() > 256 || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
			return "", Fail("bootstrap_failed", "The existing bootstrap credential file is unsafe.", 500)
		}
		data, e := os.ReadFile(path)
		token := strings.TrimSpace(string(data))
		if e != nil || len(token) < 32 {
			return "", Fail("bootstrap_failed", "The existing bootstrap credential file is invalid.", 500)
		}
		return token, nil
	}
	if err != nil {
		return "", Fail("bootstrap_failed", "The bootstrap credential file could not be created.", 500)
	}
	defer f.Close()
	token := NewID("fc_admin")
	if _, err = f.WriteString(token + "\n"); err != nil {
		return "", Fail("bootstrap_failed", "The bootstrap credential could not be saved.", 500)
	}
	if err = f.Sync(); err != nil {
		return "", Fail("bootstrap_failed", "The bootstrap credential could not be saved durably.", 500)
	}
	return token, nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func storageError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Fail("overloaded", "The operation deadline or cancellation prevented completion.", 503)
	}
	// Driver codes are inspected without returning SQL or private values.
	var coded interface{ Code() int }
	if errors.As(err, &coded) && (coded.Code()&255 == 5 || coded.Code()&255 == 6) {
		return Fail("overloaded", "Storage is busy; retry the same mutation key later.", 503)
	}
	return Fail("storage_unavailable", "The storage operation could not be completed.", 500)
}

const writeTimeout = 2 * time.Second

var schema = []string{
	`CREATE TABLE metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL)`,
	`CREATE TABLE projects(id TEXT PRIMARY KEY,name TEXT NOT NULL,github_repository_id INTEGER NOT NULL UNIQUE CHECK(github_repository_id>0),repository TEXT NOT NULL)`,
	`CREATE TABLE workers(project_id TEXT NOT NULL REFERENCES projects(id),id TEXT NOT NULL,name TEXT NOT NULL,PRIMARY KEY(project_id,id),UNIQUE(project_id,name))`,
	`CREATE TABLE credentials(id TEXT PRIMARY KEY,principal_id TEXT NOT NULL UNIQUE,token_sha256 TEXT NOT NULL UNIQUE,is_admin INTEGER NOT NULL CHECK(is_admin IN(0,1)),revoked INTEGER NOT NULL CHECK(revoked IN(0,1)))`,
	`CREATE TABLE grants(credential_id TEXT NOT NULL REFERENCES credentials(id),project_id TEXT NOT NULL REFERENCES projects(id),can_read INTEGER NOT NULL CHECK(can_read IN(0,1)),can_write INTEGER NOT NULL CHECK(can_write IN(0,1)),PRIMARY KEY(credential_id,project_id))`,
	`CREATE TABLE receipts(principal_id TEXT NOT NULL,project_id TEXT NOT NULL,operation TEXT NOT NULL,target TEXT NOT NULL,key TEXT NOT NULL,fingerprint TEXT NOT NULL,response_json TEXT NOT NULL,PRIMARY KEY(principal_id,project_id,operation,target,key))`,
	`CREATE TABLE events(sequence INTEGER PRIMARY KEY AUTOINCREMENT,project_id TEXT NOT NULL REFERENCES projects(id),event_type TEXT NOT NULL,resource_id TEXT NOT NULL,revision INTEGER NOT NULL,actor_id TEXT NOT NULL,payload_json TEXT NOT NULL,created_at TEXT NOT NULL)`,
	`CREATE INDEX events_project_sequence ON events(project_id,sequence)`,
}
