// Command build-vector writes the deterministic public TEST-ONLY dist recipe.
// It supplies input archives and templates; component-manifest build derives the
// golden document from those inputs without using its expected archive digests.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/witwave-ai/witself/internal/localcell/trust"
)

func main() {
	metadata := flag.String("metadata", "", "positive-base.json input template")
	out := flag.String("out", "", "new fixture dist directory")
	flag.Parse()
	if err := writeDist(*metadata, *out); err != nil {
		fmt.Fprintln(os.Stderr, "fixture recipe failed")
		os.Exit(1)
	}
}

func writeDist(metadata, out string) error {
	if metadata == "" || out == "" || flag.NArg() != 0 {
		return errors.New("invalid arguments")
	}
	raw, err := os.ReadFile(metadata)
	if err != nil {
		return err
	}
	var m trust.ComponentSet
	if json.Unmarshal(raw, &m) != nil || len(m.Platforms) != 1 {
		return errors.New("invalid fixture metadata")
	}
	p := m.Platforms[0]
	roles := []string{"app", "service_host", "server", "worker", "postgresql"}
	if p.OS != "linux" || p.Arch != "amd64" || len(p.Components) != len(roles) {
		return errors.New("invalid fixture platform")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		return err
	}
	for i, role := range roles {
		c := p.Components[i]
		name := "bin/" + role
		if c.Role != role || c.Archive.Name != role+".tar.gz" || c.Archive.Format != "tar.gz" || len(c.Inventory) != 1 || c.Inventory[0].Path != name {
			return errors.New("invalid fixture component")
		}
		archive, err := archiveBytes(name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, c.Archive.Name), archive, 0600); err != nil {
			return err
		}
	}
	platform, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "component-metadata.json"), raw, 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "platform.json"), platform, 0600)
}

func archiveBytes(name string) ([]byte, error) {
	var out bytes.Buffer
	gz, err := gzip.NewWriterLevel(&out, gzip.NoCompression)
	if err != nil {
		return nil, err
	}
	// Stored DEFLATE blocks avoid compression heuristic changes. No optional
	// gzip fields are present; the MTIME bytes are zero and OS is unknown.
	gz.Header = gzip.Header{ModTime: time.Unix(0, 0).UTC(), OS: 255}
	tw := tar.NewWriter(gz)
	body := []byte("data")
	// Each archive contains exactly this one regular member, then USTAR end
	// blocks. No directory members, links, PAX records or owner names exist.
	h := &tar.Header{Name: name, Mode: 0755, Size: int64(len(body)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR, Uid: 0, Gid: 0, ModTime: time.Unix(0, 0).UTC()}
	if err := tw.WriteHeader(h); err != nil {
		return nil, err
	}
	if _, err := tw.Write(body); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
