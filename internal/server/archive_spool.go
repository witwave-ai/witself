package server

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// createArchiveSpool keeps local file creation anchored to the startup handle,
// even if the configured path is later renamed or replaced.
func (cfg Config) createArchiveSpool(pattern string) (*os.File, error) {
	if cfg.SpoolRoot == nil {
		if cfg.LocalRuntime || cfg.SpoolDir != "" {
			return nil, errors.New("local archive spool directory is unavailable")
		}
		return os.CreateTemp("", pattern)
	}
	for range 100 {
		var random [4]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		name := strings.Replace(pattern, "*", strconv.FormatUint(uint64(binary.LittleEndian.Uint32(random[:])), 10), 1)
		file, err := cfg.SpoolRoot.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return file, err
	}
	return nil, errors.New("local archive spool name allocation failed")
}

func (cfg Config) removeArchiveSpool(file *os.File) error {
	if cfg.SpoolRoot != nil {
		return cfg.SpoolRoot.Remove(filepath.Base(file.Name()))
	}
	return os.Remove(file.Name())
}
