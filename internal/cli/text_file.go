package cli

import (
	"io"
	"os"
	"runtime"
	"unicode/utf8"
)

type textFileFailure uint8

const (
	textFileOK textFileFailure = iota
	textFileInvalid
	textFilePermissions
)

func readPrivateTextFile(path string, maxBytes int) (string, bool) {
	value, failure := readPrivateTextFileChecked(path, maxBytes)
	return value, failure == textFileOK
}

// Inspect before and after opening to reject accidental FIFO/device reads and
// permissive files. This does not claim protection against hostile filesystem races.
func readPrivateTextFileChecked(path string, maxBytes int) (string, textFileFailure) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", textFileInvalid
	}
	if failure := privateTextFileInfo(info, maxBytes); failure != textFileOK {
		return "", failure
	}
	f, err := os.Open(path)
	if err != nil {
		return "", textFileInvalid
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", textFileInvalid
	}
	if failure := privateTextFileInfo(opened, maxBytes); failure != textFileOK {
		return "", failure
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil || len(data) > maxBytes || !utf8.Valid(data) {
		return "", textFileInvalid
	}
	return string(data), textFileOK
}

func privateTextFileInfo(info os.FileInfo, maxBytes int) textFileFailure {
	if !info.Mode().IsRegular() || info.Size() > int64(maxBytes) {
		return textFileInvalid
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return textFilePermissions
	}
	return textFileOK
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

func loadTextFiles(values map[string]string, limits map[string]int) (bool, *diagnostic) {
	for flag, limit := range limits {
		if path, file := values[flag+"-file"]; file {
			value, failure := readPrivateTextFileChecked(path, limit)
			if failure == textFilePermissions {
				return false, &diagnostic{
					Code:    "invalid_arguments",
					Message: "The file supplied for " + flag + "-file has unsafe permissions; group and other access must be disabled.",
					Action:  "Set the supplied file's permissions to 0600 (chmod 600), then retry with the same file contents and original --key. No request was sent.",
				}
			}
			if failure != textFileOK {
				return false, nil
			}
			values[flag] = value
		}
	}
	return true, nil
}
