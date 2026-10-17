package protocol

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxFileBytes = 10 << 20
const MaxFileCount = 1

// File contains immutable document bytes and an untrusted original basename.
// A single base64 encoded 10 MiB file fits the 16 MiB transport frame limit.
type File struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

func ValidateFiles(files []File) error {
	if len(files) > MaxFileCount {
		return errors.New("too many input files")
	}
	for _, file := range files {
		if len(file.Data) > MaxFileBytes {
			return errors.New("input file size is invalid")
		}
		if strings.TrimSpace(file.Name) == "" || len(file.Name) > 1024 || !utf8.ValidString(file.Name) ||
			strings.ContainsAny(file.Name, "/\\") || strings.IndexFunc(file.Name, unicode.IsControl) >= 0 || file.Name == "." || file.Name == ".." {
			return errors.New("input file name is invalid")
		}
	}
	return nil
}
