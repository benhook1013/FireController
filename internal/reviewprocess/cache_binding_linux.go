//go:build linux

package reviewprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// CacheMaintenanceBinding is a generation-owner opt-in, not an ownership
// discovery mechanism. The release owner must qualify the trusted writer set.
// No daemon constructor or cleanup caller installs this draft yet.
type CacheMaintenanceBinding struct {
	path, source, config, home string
}

type cacheBindingDocument struct {
	Version             int    `json:"version"`
	State               string `json:"state"`
	Policy              string `json:"policy"`
	BootID              string `json:"boot_id"`
	CGroup              string `json:"cgroup"`
	Source              string `json:"source"`
	ConfigurationSHA256 string `json:"configuration_sha256"`
	OwnerEvidenceSHA256 string `json:"owner_evidence_sha256"`
	Home                string `json:"home"`
	HomeDevice          uint64 `json:"home_device"`
	HomeInode           uint64 `json:"home_inode"`
	HomeUID             uint32 `json:"home_uid"`
	HomeGID             uint32 `json:"home_gid"`
}

func bindingHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func NewCacheMaintenanceBinding(path, source, configurationSHA256, home string) (*CacheMaintenanceBinding, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !filepath.IsAbs(home) || filepath.Clean(home) != home || !bindingHex(source, 40) || !bindingHex(configurationSHA256, 64) {
		return nil, errors.New("cache generation binding unavailable")
	}
	return &CacheMaintenanceBinding{path: path, source: source, config: configurationSHA256, home: home}, nil
}

// protectedBinding reads each generation afresh, without following links. Every
// parent and the file must be root-owned and not writable by producer UIDs.
// The entire containing directory must be projected read-only; a single-file
// mount is disallowed by the qualified deployment contract, not inferred here.
func bindingDirectoryProjection(mountinfo []byte, path string) bool {
	parent := filepath.Dir(path)
	found := false
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(string(mountinfo), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		mount := unescape.Replace(fields[4])
		if mount == path {
			return false
		} // A pinned single-file bind hides atomic replacements.
		if mount == parent {
			for _, opt := range strings.Split(fields[5], ",") {
				if opt == "ro" {
					found = true
				}
			}
		}
	}
	return found
}

func protectedBinding(path string) ([]byte, error) {
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil || !bindingDirectoryProjection(mountinfo, path) {
		return nil, errors.New("cache binding requires exact read-only directory projection")
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		if i == len(parts)-1 {
			var parentFS syscall.Statfs_t
			if e := syscall.Fstatfs(fd, &parentFS); e != nil || parentFS.Flags&1 == 0 {
				syscall.Close(fd)
				return nil, errors.New("cache binding directory projection is not read-only")
			}
		}
		flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC | syscall.O_NONBLOCK
		if i < len(parts)-1 {
			flags |= syscall.O_DIRECTORY
		}
		next, e := syscall.Openat(fd, part, flags, 0)
		syscall.Close(fd)
		if e != nil {
			return nil, e
		}
		fd = next
		var st syscall.Stat_t
		if e = syscall.Fstat(fd, &st); e != nil || st.Uid != 0 || st.Mode&0022 != 0 {
			syscall.Close(fd)
			return nil, errors.New("unprotected cache binding path")
		}
		if i == len(parts)-1 && (st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Nlink != 1 || st.Size <= 0 || st.Size > 16384) {
			syscall.Close(fd)
			return nil, errors.New("invalid cache binding file")
		}
	}
	f := os.NewFile(uintptr(fd), "cache-generation-binding")
	defer f.Close()
	var fs syscall.Statfs_t
	if err = syscall.Fstatfs(fd, &fs); err != nil || fs.Flags&1 == 0 {
		return nil, errors.New("cache binding projection is not read-only")
	}
	b, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(b) > 16384 {
		return nil, errors.New("cache binding read unavailable")
	}
	return b, nil
}

func uniqueBindingJSON(b []byte) error {
	allowed := map[string]bool{"version": true, "state": true, "policy": true, "boot_id": true, "cgroup": true, "source": true, "configuration_sha256": true, "owner_evidence_sha256": true, "home": true, "home_device": true, "home_inode": true, "home_uid": true, "home_gid": true}
	d := json.NewDecoder(bytes.NewReader(b))
	var value func(int) error
	value = func(depth int) error {
		if depth > 4 {
			return errors.New("binding depth")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		if delim != '{' || depth != 0 {
			return errors.New("binding shape")
		}
		seen := map[string]bool{}
		for d.More() {
			key, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := key.(string)
			if !ok || !allowed[s] || seen[s] || len(seen) >= len(allowed) {
				return errors.New("duplicate or excessive binding fields")
			}
			seen[s] = true
			if e = value(depth + 1); e != nil {
				return e
			}
		}
		if len(seen) != len(allowed) {
			return errors.New("incomplete binding fields")
		}
		_, err = d.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing binding data")
	}
	return nil
}

// Check must run before and again after exclusive HOME custody acquisition.
// Revocation may permit an already-admitted maintenance operation to finish;
// its owner must wait for the exclusive HOME lease before introducing a writer.
func (b *CacheMaintenanceBinding) Check(ctx context.Context) error {
	if b == nil || ctx == nil {
		return errors.New("cache generation binding unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := protectedBinding(b.path)
	if err != nil {
		return err
	}
	if err = uniqueBindingJSON(raw); err != nil {
		return err
	}
	var doc cacheBindingDocument
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&doc); err != nil {
		return err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	group, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}
	if doc.Version != 1 || doc.State != "enabled" || doc.Policy != "exclusive-home-v1" || doc.Source != b.source || doc.ConfigurationSHA256 != b.config || !bindingHex(doc.OwnerEvidenceSHA256, 64) || doc.Home != b.home || doc.BootID == "" || doc.BootID != strings.TrimSpace(string(boot)) || doc.CGroup == "" || doc.CGroup != strings.TrimSpace(string(group)) {
		return errors.New("cache generation binding mismatch")
	}
	fd, err := syscall.Open(b.home, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err = syscall.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Dev != doc.HomeDevice || st.Ino != doc.HomeInode || st.Uid != doc.HomeUID || st.Gid != doc.HomeGID || st.Uid != uint32(os.Geteuid()) || st.Mode&0077 != 0 {
		return errors.New("cache HOME binding mismatch")
	}
	return ctx.Err()
}
