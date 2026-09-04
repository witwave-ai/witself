// Package tokenfile reads whitespace-delimited credentials from files with a
// shared strict-empty policy and caller-compatible error context.
package tokenfile

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Options controls the intentional differences between token-file consumers.
// The zero value requires the file and quotes its path in errors. UnquotedPath
// exists only to preserve established command error text.
type Options struct {
	Description  string
	AllowMissing bool
	UnquotedPath bool
}

// Read loads and trims one token. Empty or whitespace-only files are rejected.
func Read(path string, options Options) (string, error) {
	description := options.Description
	if description == "" {
		description = "token file"
	}
	renderedPath := strconv.Quote(path)
	if options.UnquotedPath {
		renderedPath = path
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		if options.AllowMissing && errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s %s: %w", description, renderedPath, err)
	}
	token := strings.TrimSpace(string(contents))
	if token == "" {
		return "", fmt.Errorf("%s %s is empty", description, renderedPath)
	}
	return token, nil
}
