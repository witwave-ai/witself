package main

// witself-infra config remove-cell -cell NAME — remove one inventory entry
// after a destroy or an aborted provisioning, without hand-editing YAML.

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"gopkg.in/yaml.v3"

	"github.com/witwave-ai/witself/infra/pulumi/internal/cell"
	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

// removeCellFlags are the only flags config remove-cell accepts. The control
// plane, the fleet token and the state backend come from the inventory.
var removeCellFlags = map[string]bool{"cell": true, "config": true, "dry-run": true, "force": true}

// configRemoveCell removes one cell's entry from the inventory. Unless -force
// is given, it first refuses while the cell's control plane lists the cell or
// while the cell's stack holds a resource or a pending operation. It reads
// the file once, before parsing it, copies exactly those bytes to
// <file>.bak-<UTC time> with mode 0600, and writes nothing if the file no
// longer holds them. It never changes Pulumi state, a state directory or a
// kube context. With -dry-run it runs every check, prints the entry it would
// remove and writes nothing.
func configRemoveCell(fs *flag.FlagSet, configPath string, out, errOut io.Writer) error {
	value := func(name string) string {
		if f := fs.Lookup(name); f != nil {
			return f.Value.String()
		}
		return ""
	}
	var extra []string
	fs.Visit(func(f *flag.Flag) {
		if !removeCellFlags[f.Name] {
			extra = append(extra, "-"+f.Name)
		}
	})
	if len(extra) > 0 {
		return fmt.Errorf("config remove-cell takes only -cell, -config, -force and -dry-run (got %s); the control plane, the fleet token and the state backend come from the inventory", strings.Join(extra, ", "))
	}
	cellName := value("cell")
	if cellName == "" {
		return fmt.Errorf("config remove-cell requires -cell NAME")
	}
	force := value("force") == "true"
	dryRun := value("dry-run") == "true"

	path, err := resolveConfigPath(configPath)
	if err != nil {
		return err
	}
	// Read the bytes before parsing them: the backup holds exactly these
	// bytes, and the file must still hold them when the rewrite is written.
	original, readErr := os.ReadFile(path)
	cfg, _, err := loadInfraConfig(path)
	if err != nil {
		return err
	}
	if readErr != nil {
		return readErr
	}
	entry, ok := cfg.Cells[cellName]
	if !ok {
		names := make([]string, 0, len(cfg.Cells))
		for n := range cfg.Cells {
			names = append(names, n)
		}
		sort.Strings(names)
		return fmt.Errorf("cell %q not in %s (have: %s)", cellName, path, strings.Join(names, ", "))
	}

	ctx := context.Background()
	if force {
		_, _ = fmt.Fprintf(errOut, "warning: -force skipped the fleet registry and stack checks for cell %q\n", cellName)
	} else {
		if err := checkRemovedCellRegistry(ctx, cfg, cellName, os.Getenv, out); err != nil {
			return err
		}
		if err := checkRemovedCellStack(ctx, cellName, entry, os.Getenv, out); err != nil {
			return err
		}
	}

	delete(cfg.Cells, cellName)
	rewritten, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := refuseCredentialLines(path, rewritten); err != nil {
		return err
	}
	if dryRun {
		entryYAML, err := yaml.Marshal(map[string]cellEntry{cellName: entry})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "dry run: would remove cell %s from %s; nothing was written\n", cellName, path)
		_, _ = fmt.Fprint(out, string(entryYAML))
		_, _ = fmt.Fprintln(out, "note: without -dry-run, remove-cell copies the file to a backup and rewrites it — YAML comments are not preserved")
		return nil
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, original) {
		return fmt.Errorf("remove-cell: %s changed while remove-cell was checking; nothing was written; run it again", path)
	}
	backupPath := path + ".bak-" + time.Now().UTC().Format("20060102T150405Z")
	if err := writeInventoryBackup(backupPath, original); err != nil {
		return err
	}
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "removed cell %s from %s\n", cellName, path)
	_, _ = fmt.Fprintf(out, "backup: %s\n", backupPath)
	_, _ = fmt.Fprintln(out, "note: remove-cell rewrites the file — YAML comments are not preserved; the backup holds the original bytes")
	return nil
}

// checkRemovedCellRegistry refuses while the control plane that health probes
// for the cell lists it under its registry name or its inventory name, and
// whenever that listing cannot be read or trusted. A cell with no control
// plane in its entry or in defaults is not checked.
func checkRemovedCellRegistry(ctx context.Context, cfg *infraConfig, cellName string, getenv func(string) string, out io.Writer) error {
	entry := cfg.Cells[cellName]
	controlPlane, tokenFile := effectiveHealthConnection(entry, cfg.Defaults)
	if controlPlane == "" {
		_, _ = fmt.Fprintf(out, "registry: no control plane is configured for cell %q; not checked\n", cellName)
		return nil
	}
	names := []string{entry.registryName(cellName)}
	if names[0] != cellName {
		names = append(names, cellName)
	}
	client, err := fleet.NewClient(controlPlane, tokenFile)
	var cells []fleet.Cell
	if err == nil {
		cells, err = client.ListCells(ctx)
	}
	if err != nil {
		return fmt.Errorf("remove-cell: cannot read the fleet registry of control plane %s for cell %q: %v; refusing to remove the entry", controlPlane, cellName, boundRefusalDetail(redactDiagnostic(err.Error(), getenv)))
	}
	// ListCells accepts any 200 answer, even one without a cell list. A
	// registry in use lists at least one cell, and every cell has a name.
	nameless := slices.ContainsFunc(cells, func(c fleet.Cell) bool { return c.Name == "" })
	if len(cells) == 0 || nameless {
		return fmt.Errorf("remove-cell: cannot read the fleet registry of control plane %s for cell %q: the answer lists no cells or a cell without a name; refusing to remove the entry", controlPlane, cellName)
	}
	for _, c := range cells {
		if slices.Contains(names, c.Name) {
			return fmt.Errorf("remove-cell: control plane %s lists fleet registry entry %q for cell %q; refusing to remove the entry (destroy the cell first; a cell that destroy removed moments ago can stay listed briefly)", controlPlane, c.Name, cellName)
		}
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(n)
	}
	_, _ = fmt.Fprintf(out, "registry: %s lists no entry named %s\n", controlPlane, strings.Join(quoted, " or "))
	return nil
}

const maxRefusalDetailBytes = 512

func refusalDetailBreak(r rune) bool {
	return r != '\t' && (unicode.IsControl(r) || r == '\u2028' || r == '\u2029')
}

// boundRefusalDetail keeps one bounded line of already-redacted diagnostic text.
// It cuts at the first line break or other control character (a tab excepted)
// or Unicode line separator, so a remote body cannot add lines or move the cursor.
func boundRefusalDetail(text string) string {
	truncated := false
	if end := strings.IndexFunc(text, refusalDetailBreak); end >= 0 {
		text = text[:end]
		truncated = true
	}
	text = strings.TrimSpace(text)
	if len(text) > maxRefusalDetailBytes {
		end := maxRefusalDetailBytes
		for end > 0 && !utf8.RuneStart(text[end]) {
			end--
		}
		text = text[:end]
		truncated = true
	}
	if text == "" {
		text = "(no detail)"
	}
	if truncated {
		text += " [truncated]"
	}
	return text
}

// checkRemovedCellStack refuses while the cell's stack holds a resource or a
// pending operation, the same test destroy uses to declare a stack empty. It
// reads the backend the entry names, as outputs does, but only selects: it
// never creates a stack, a state directory or a passphrase. An absent stack
// passes. Only the local and r2 backends are read.
func checkRemovedCellStack(ctx context.Context, cellName string, entry cellEntry, getenv func(string) string, out io.Writer) error {
	kind := "s3"
	if entry.Backend != nil {
		kind = *entry.Backend
	}
	cannot := func(err error) error {
		return fmt.Errorf("remove-cell: cannot check the stack of cell %q: %v; refusing to remove the entry", cellName, err)
	}
	absent := func() error {
		_, _ = fmt.Fprintf(out, "stack: cell %q has no stack in its %s backend\n", cellName, kind)
		return nil
	}
	var opts []auto.LocalWorkspaceOption
	switch kind {
	case "r2":
		bucket, endpoint := "", ""
		if entry.R2Bucket != nil {
			bucket = *entry.R2Bucket
		}
		if entry.R2Endpoint != nil {
			endpoint = *entry.R2Endpoint
		}
		settings, secrets, err := resolveR2Backend("r2", "civo", bucket, endpoint, defaultStateDir(), getenv)
		if err != nil {
			return cannot(err)
		}
		env, command, err := prepareR2Workspace(ctx, "outputs", settings, secrets, cellName, io.Discard)
		if err != nil {
			return cannot(err)
		}
		opts = append(opts, auto.Pulumi(command), auto.EnvVars(env))
	case "local":
		stateDir := defaultStateDir()
		if entry.StateDir != nil {
			stateDir = *entry.StateDir
		}
		_, statErr := os.Stat(filepath.Join(stateDir, ".pulumi"))
		if errors.Is(statErr, os.ErrNotExist) {
			return absent()
		}
		if statErr != nil {
			return cannot(statErr)
		}
		env := map[string]string{"PULUMI_BACKEND_URL": "file://" + stateDir}
		passphrase := getenv("PULUMI_CONFIG_PASSPHRASE")
		if passphrase == "" {
			if b, err := os.ReadFile(filepath.Join(stateDir, "passphrase")); err == nil {
				passphrase = strings.TrimSpace(string(b))
			}
		}
		if passphrase != "" {
			rememberLocalPassphrase(passphrase)
			env["PULUMI_CONFIG_PASSPHRASE"] = passphrase
		}
		// The same Pulumi CLI seam as the r2 backend, without its version floor.
		command, _, _, _, err := r2PulumiCLI()
		if err != nil {
			return cannot(err)
		}
		opts = append(opts, auto.Pulumi(command), auto.EnvVars(env))
	default:
		return fmt.Errorf("remove-cell: cell %q uses backend %s; only a local or r2 stack can be checked; refusing to remove the entry", cellName, kind)
	}
	stack, err := auto.SelectStackInlineSource(ctx, cellName, projectName, cell.Program, opts...)
	if auto.IsSelectStack404Error(err) {
		return absent()
	}
	if err != nil {
		return cannot(err)
	}
	deployment, err := stack.Export(ctx)
	if err != nil {
		// Export runs with --show-secrets: its error carries decrypted state.
		return fmt.Errorf("remove-cell: cannot read the stack of cell %q: stack export failed; refusing to remove the entry", cellName)
	}
	if deployment.Version != 3 && deployment.Version != 4 {
		return fmt.Errorf("remove-cell: cannot read the stack of cell %q: unsupported exported deployment schema %d; refusing to remove the entry", cellName, deployment.Version)
	}
	resources, pending, err := remainingPulumiResources(deployment.Deployment)
	if err != nil {
		return fmt.Errorf("remove-cell: cannot read the stack of cell %q: the exported deployment does not decode; refusing to remove the entry", cellName)
	}
	if len(resources) > 0 || len(pending) > 0 {
		return fmt.Errorf("remove-cell: the stack of cell %q holds %d resource(s) and %d pending operation(s); refusing to remove the entry (destroy the cell first)", cellName, len(resources), len(pending))
	}
	_, _ = fmt.Fprintf(out, "stack: the stack of cell %q holds no resources and no pending operations\n", cellName)
	return nil
}

// writeInventoryBackup writes data to a new file at path with mode 0600. It
// never replaces an existing file.
func writeInventoryBackup(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("write backup %s: %w", path, err)
	}
	_, writeErr := f.Write(data)
	chmodErr := f.Chmod(0o600)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, chmodErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write backup %s: %w", path, err)
	}
	return nil
}
