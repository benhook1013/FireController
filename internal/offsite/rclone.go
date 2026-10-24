package offsite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A narrow transport makes failure/uncertain-result behavior independently
// testable. Production always uses the explicit rclone executable/configuration.
type transport interface {
	read(context.Context, string) ([]byte, error)
	put(context.Context, string, string) error
	get(context.Context, string, string) error
	remove(context.Context, string) error
}

var notFound = errors.New("offsite object absent")

type rclone struct {
	c    Config
	work string
}
type boundedOutput struct {
	buffer   bytes.Buffer
	limit    int
	stop     context.CancelFunc
	exceeded bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		b.exceeded = true
		if b.stop != nil {
			b.stop()
		}
		return 0, io.ErrShortBuffer
	}
	return b.buffer.Write(p)
}

func newTransport(c Config, work string) (transport, error) {
	if _, err := privateRead(c.RcloneConfig, 1<<20); err != nil {
		return nil, err
	}
	s, err := os.Stat(c.RcloneBinary)
	if err != nil || !s.Mode().IsRegular() {
		return nil, failure("offsite_configuration_invalid")
	}
	return &rclone{c: c, work: work}, nil
}

func (r *rclone) run(ctx context.Context, args ...string) ([]byte, error) {
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	base := []string{"--config", r.c.RcloneConfig, "--ask-password=false", "--retries", "1", "--low-level-retries", "2", "--contimeout", "10s", "--timeout", "30s", "--stats", "0", "--log-level", "ERROR", "--max-transfer", strconv.FormatInt(r.c.MaxBundleBytes, 10), "--cutoff-mode", "HARD"}
	cmd := exec.CommandContext(child, r.c.RcloneBinary, append(base, args...)...)
	cmd.Dir = r.work
	cmd.WaitDelay = 2 * time.Second
	processGroup(cmd)
	// Provider options, alternate remotes and interactive password commands are
	// not inherited from unrelated agent/session environment variables.
	for _, key := range []string{"PATH", "SystemRoot", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+r.work, "XDG_CACHE_HOME="+r.work, "TMPDIR="+r.work, "LANG=C.UTF-8", "NO_COLOR=1")
	out := &boundedOutput{limit: 32 << 10, stop: cancel}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if out.exceeded {
		return nil, failure("offsite_output_limit")
	}
	if err != nil {
		var e *exec.ExitError
		if errors.As(err, &e) && (e.ExitCode() == 3 || e.ExitCode() == 4) {
			return nil, notFound
		}
		if ctx.Err() != nil {
			return nil, failure("offsite_cancelled")
		}
		return nil, failure("offsite_transport_failed")
	}
	return out.buffer.Bytes(), nil
}
func (r *rclone) read(ctx context.Context, name string) ([]byte, error) {
	// S3 can report an absent key as a virtual directory to --stat, and cat
	// can succeed with no files. A filtered parent listing distinguishes that
	// absence from a corrupt zero-byte receipt without listing archive bodies.
	i := strings.LastIndexByte(name, '/')
	if i < 0 {
		return nil, failure("offsite_configuration_invalid")
	}
	b, err := r.run(ctx, "lsjson", name[:i], "--files-only", "--max-depth", "1", "--include", name[i+1:])
	if err != nil {
		return nil, err
	}
	var matches []struct {
		Name  string
		Size  int64
		IsDir bool
	}
	if json.Unmarshal(b, &matches) != nil {
		return nil, failure("offsite_remote_conflict")
	}
	if len(matches) == 0 {
		return nil, notFound
	}
	if len(matches) != 1 || matches[0].Name != name[i+1:] || matches[0].IsDir || matches[0].Size <= 0 || matches[0].Size > 4096 {
		return nil, failure("offsite_remote_conflict")
	}
	b, err = r.run(ctx, "cat", name)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != matches[0].Size {
		return nil, failure("offsite_remote_conflict")
	}
	return b, nil
}
func (r *rclone) put(ctx context.Context, source, name string) error {
	_, err := r.run(ctx, "copyto", source, name, "--ignore-times")
	return err
}
func (r *rclone) get(ctx context.Context, name, destination string) error {
	if !filepath.IsAbs(destination) {
		return failure("offsite_configuration_invalid")
	}
	_, err := r.run(ctx, "copyto", name, destination, "--ignore-times")
	if err == nil {
		err = os.Chmod(destination, 0600)
	}
	return err
}
func (r *rclone) remove(ctx context.Context, name string) error {
	_, err := r.run(ctx, "deletefile", name)
	if errors.Is(err, notFound) {
		return nil
	}
	return err
}
