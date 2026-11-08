package localcell

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var serviceSystemdHex = regexp.MustCompile(`(?i)[0-9a-f]{64}`)
var serviceSystemdToken = regexp.MustCompile(`[A-Za-z0-9_+/-]{43,}={0,2}`)
var serviceSystemdEnvironment = regexp.MustCompile(`(?m)\b(?:Environment(?:File)?|[A-Z_][A-Z0-9_]*)[ \t]*=[^\r\n]*`)

// Sanitize before taking the tail: truncating first can expose a partial token
// or environment assignment. No environment or credential file is read here.
func sanitizeServiceSystemdDetail(text, root, home string, privatePaths ...string) string {
	if root != "" {
		text = strings.ReplaceAll(text, root, "<root>")
	}
	if home != "" {
		text = strings.ReplaceAll(text, home, "<home>")
	}
	for _, path := range privatePaths {
		if path != "" {
			text = strings.ReplaceAll(text, path, "<path>")
		}
	}
	text = serviceSystemdEnvironment.ReplaceAllString(text, "<environment>")
	text = serviceSystemdHex.ReplaceAllString(text, "<hex>")
	// Cover standard and URL-safe base64 after private path replacement so the
	// placeholders survive. Long plain path segments may also become <token>;
	// privacy takes precedence over retaining that diagnostic detail.
	text = serviceSystemdToken.ReplaceAllString(text, "<token>")
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	text = strings.TrimSpace(text)
	if len(text) > 400 {
		text = text[len(text)-400:]
		for len(text) > 0 && !utf8.RuneStart(text[0]) {
			text = text[1:]
		}
	}
	return text
}

func serviceSystemdError(reason error, step, detail string) error {
	return fmt.Errorf("%w: %s: %s", reason, step, detail)
}

func serviceSystemdCommandError(reason error, step string, err error, root, home string, privatePaths ...string) error {
	// A filesystem refusal needs the operation/cause, not a potentially external
	// XDG path. This also protects the directory walk before a descriptor exists.
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err
	}
	detail := err.Error()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) != 0 {
		detail = string(exitErr.Stderr)
	}
	detail = sanitizeServiceSystemdDetail(detail, root, home, privatePaths...)
	if detail == "" {
		detail = "command_failed"
	}
	return serviceSystemdError(reason, step, detail)
}
