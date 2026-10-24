package cli

import (
	"fmt"
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
	textFileTooLarge
)

func readPrivateTextFile(path string, maxBytes int) (string, bool) {
	value, failure := readPrivateTextFileChecked(path, maxBytes)
	return value, failure == textFileOK
}

// Inspect before and after opening to reject accidental FIFO/device reads and
// permissive files. This does not claim protection against hostile filesystem races.
func readPrivateTextFileChecked(path string, maxBytes int) (string, textFileFailure) {
	value, failure, _ := readPrivateTextFileMeasured(path, maxBytes)
	return value, failure
}

func readPrivateTextFileMeasured(path string, maxBytes int) (string, textFileFailure, int64) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", textFileInvalid, 0
	}
	if failure := privateTextFileInfo(info, maxBytes); failure != textFileOK {
		return "", failure, info.Size()
	}
	f, err := os.Open(path)
	if err != nil {
		return "", textFileInvalid, 0
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", textFileInvalid, 0
	}
	if failure := privateTextFileInfo(opened, maxBytes); failure != textFileOK {
		return "", failure, opened.Size()
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil || len(data) > maxBytes || !utf8.Valid(data) {
		return "", textFileInvalid, 0
	}
	return string(data), textFileOK, int64(len(data))
}

func privateTextFileInfo(info os.FileInfo, maxBytes int) textFileFailure {
	if !info.Mode().IsRegular() {
		return textFileInvalid
	}
	if info.Size() > int64(maxBytes) {
		return textFileTooLarge
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
			value, failure, size := readPrivateTextFileMeasured(path, limit)
			if failure == textFileTooLarge {
				return false, &diagnostic{
					Code:    "invalid_arguments",
					Message: fmt.Sprintf("The file supplied for %s-file is %d bytes; the limit is %d bytes.", flag, size, limit),
					Action:  fmt.Sprintf("Reduce the supplied file to at most %d UTF-8 bytes, then retry. No request was sent.", limit),
				}
			}
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
