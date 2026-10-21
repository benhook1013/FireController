package cli

import (
	"io"
	"os"
	"runtime"
	"unicode/utf8"
)

// Inspect before and after opening to reject accidental FIFO/device reads and
// permissive files. This does not claim protection against hostile filesystem races.
func readPrivateTextFile(path string, maxBytes int) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil || !privateTextFileInfo(info, maxBytes) {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !privateTextFileInfo(opened, maxBytes) || !os.SameFile(info, opened) {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil || len(data) > maxBytes || !utf8.Valid(data) {
		return "", false
	}
	return string(data), true
}

func privateTextFileInfo(info os.FileInfo, maxBytes int) bool {
	return info.Mode().IsRegular() && info.Size() <= int64(maxBytes) && (runtime.GOOS == "windows" || info.Mode().Perm()&0077 == 0)
}

// Reserve supplied file-backed fields before validating the rest of the request.
// Empty placeholders avoid consuming a valid combined job-progress budget;
// messages need a nonblank placeholder for their required body.
func prepareTextFiles(values map[string]string, limits map[string]int) bool {
	for flag := range limits {
		if path, file := values[flag+"-file"]; file {
			if _, inline := values[flag]; inline || path == "" {
				return false
			}
			values[flag] = ""
			if flag == "--body" {
				values[flag] = "file validation"
			}
		}
	}
	return true
}

func loadTextFiles(values map[string]string, limits map[string]int) bool {
	for flag, limit := range limits {
		if path, file := values[flag+"-file"]; file {
			value, ok := readPrivateTextFile(path, limit)
			if !ok {
				return false
			}
			values[flag] = value
		}
	}
	return true
}
