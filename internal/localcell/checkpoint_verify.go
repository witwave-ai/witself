package localcell

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

type checkpointVerifyResult string

const checkpointIntegrityVerified checkpointVerifyResult = "integrity_verified"

type checkpointVerifyReason string

const (
	checkpointVerifyLockRequired            checkpointVerifyReason = "lock_required"
	checkpointVerifyWitnessUnreadable       checkpointVerifyReason = "witness_unreadable"
	checkpointVerifyWitnessMalformed        checkpointVerifyReason = "witness_malformed"
	checkpointVerifyFormatUnsupported       checkpointVerifyReason = "format_unsupported"
	checkpointVerifyIdentityMismatch        checkpointVerifyReason = "identity_mismatch"
	checkpointVerifyInventoryUnreadable     checkpointVerifyReason = "inventory_unreadable"
	checkpointVerifyInventoryDigestMismatch checkpointVerifyReason = "inventory_digest_mismatch"
	checkpointVerifyInventoryMalformed      checkpointVerifyReason = "inventory_malformed"
	checkpointVerifyPathRefused             checkpointVerifyReason = "path_refused"
	checkpointVerifyCountMismatch           checkpointVerifyReason = "count_mismatch"
	checkpointVerifyTreeMismatch            checkpointVerifyReason = "tree_mismatch"
	checkpointVerifyFileUnreadable          checkpointVerifyReason = "file_unreadable"
	checkpointVerifyFileMismatch            checkpointVerifyReason = "file_mismatch"
	checkpointVerifyManifestUnreadable      checkpointVerifyReason = "manifest_unreadable"
	checkpointVerifyManifestMismatch        checkpointVerifyReason = "manifest_mismatch"
	checkpointVerifyCancelled               checkpointVerifyReason = "cancelled"
)

type checkpointVerifyError struct{ reason checkpointVerifyReason }

func (checkpointVerifyError) Error() string { return "checkpoint integrity verification refused" }

// verifyCheckpoint checks only the sealed bytes of one kept copy under the
// caller's lock. No live manifest or process contributes to the result.
func (c *Cell) verifyCheckpoint(ctx context.Context, lock *operationLock, id string) (checkpointVerifyResult, error) {
	if lock == nil {
		return "", checkpointVerifyError{checkpointVerifyLockRequired}
	}
	if ctx.Err() != nil {
		return "", checkpointVerifyError{checkpointVerifyCancelled}
	}
	if err := c.validateRecovery(); err != nil {
		return "", err
	}
	dir, err := c.checkpointDir(id)
	if err != nil {
		return "", checkpointVerifyError{checkpointVerifyIdentityMismatch}
	}
	ops := c.checkpointOps()
	budget := checkpointRetryBudget{limit: checkpointRetryFloor}
	witness, inventory, err := verifyCheckpointDocuments(ctx, &budget, ops, dir, id)
	if err != nil {
		return "", err
	}
	if err := checkpointVerifyShape(inventory, witness, &budget); err != nil {
		return "", err
	}
	if err := checkpointVerifyTree(ctx, dir, inventory); err != nil {
		return "", err
	}
	for _, entry := range inventory.Entries {
		if ctx.Err() != nil {
			return "", checkpointVerifyError{checkpointVerifyCancelled}
		}
		if entry.Type != "file" {
			continue
		}
		var digest [sha256.Size]byte
		var size int64
		err := checkpointVerifyRead(ctx, &budget, ops, filepath.Join(dir, filepath.FromSlash(entry.Path)), func(r *os.File) error {
			var err error
			digest, size, err = checkpointHash(r)
			return err
		})
		if err != nil {
			return "", checkpointVerifyReadError(ctx, checkpointVerifyFileUnreadable)
		}
		if size != *entry.Size || hex.EncodeToString(digest[:]) != entry.SHA256 {
			return "", checkpointVerifyError{checkpointVerifyFileMismatch}
		}
	}
	var manifest Manifest
	var malformed bool
	err = checkpointVerifyRead(ctx, &budget, ops, filepath.Join(dir, "manifest.json"), func(r *os.File) error {
		manifest, malformed = Manifest{}, false
		decoder := json.NewDecoder(r)
		err := decoder.Decode(&manifest)
		// Syntax/type errors describe content; other errors came from the reader.
		if err != nil {
			switch err.(type) {
			case *json.SyntaxError, *json.UnmarshalTypeError, *time.ParseError:
				malformed = true
				return nil
			}
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				malformed = true
				return nil
			}
		}
		return err
	})
	if err != nil {
		return "", checkpointVerifyReadError(ctx, checkpointVerifyManifestUnreadable)
	}
	if malformed || manifest.InstallationID != witness.InstallationID || manifest.LayoutVersion != witness.LayoutVersion ||
		manifest.PrincipalLayoutVersion != witness.PrincipalLayoutVersion || manifest.LastVerifiedSchemaVersion != witness.SchemaVersion || witness.PostgresMajor != "16" {
		return "", checkpointVerifyError{checkpointVerifyManifestMismatch}
	}
	return checkpointIntegrityVerified, nil
}

// checkpointVerifyRead retries a complete read from offset zero, including
// transient read failures, without retaining the platform error in a refusal.
func checkpointVerifyRead(ctx context.Context, budget *checkpointRetryBudget, ops checkpointOperations, name string, consume func(*os.File) error) error {
	err := retryCheckpointStep(ctx, budget, ops, func() error {
		file, err := ops.open(name)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		return consume(file)
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func checkpointVerifyReadError(ctx context.Context, reason checkpointVerifyReason) error {
	if ctx.Err() != nil {
		reason = checkpointVerifyCancelled
	}
	return checkpointVerifyError{reason}
}

func verifyCheckpointDocuments(ctx context.Context, budget *checkpointRetryBudget, ops checkpointOperations, dir, id string) (w checkpointWitness, inventory checkpointInventory, err error) {
	var data []byte
	var oversized bool
	err = checkpointVerifyRead(ctx, budget, ops, filepath.Join(dir, checkpointWitnessName), func(r *os.File) error {
		data, oversized = nil, false
		var err error
		info, err := r.Stat()
		if err != nil {
			return err
		}
		oversized = info.Size() > checkpointControlLimit
		if oversized {
			return nil
		}
		data, err = io.ReadAll(io.LimitReader(r, checkpointControlLimit))
		return err
	})
	if err != nil {
		return w, inventory, checkpointVerifyReadError(ctx, checkpointVerifyWitnessUnreadable)
	}
	if oversized || !checkpointVerifyJSON(data, &w) || !checkpointVerifyDigest(w.InventorySHA256) ||
		w.Files < 0 || w.Directories < 0 || w.Bytes < 0 || w.InstallationID == "" {
		return w, inventory, checkpointVerifyError{checkpointVerifyWitnessMalformed}
	}
	if w.Format != 1 {
		return w, inventory, checkpointVerifyError{checkpointVerifyFormatUnsupported}
	}
	if w.CheckpointID != id {
		return w, inventory, checkpointVerifyError{checkpointVerifyIdentityMismatch}
	}
	var buffer bytes.Buffer
	hash := sha256.New()
	err = checkpointVerifyRead(ctx, budget, ops, filepath.Join(dir, checkpointInventoryName), func(r *os.File) error {
		buffer.Reset()
		hash.Reset()
		_, err := io.Copy(io.MultiWriter(&buffer, hash), r)
		return err
	})
	if err != nil {
		return w, inventory, checkpointVerifyReadError(ctx, checkpointVerifyInventoryUnreadable)
	}
	if hex.EncodeToString(hash.Sum(nil)) != w.InventorySHA256 {
		return w, inventory, checkpointVerifyError{checkpointVerifyInventoryDigestMismatch}
	}
	if !checkpointVerifyJSON(buffer.Bytes(), &inventory) {
		return w, inventory, checkpointVerifyError{checkpointVerifyInventoryMalformed}
	}
	if inventory.Format != 1 {
		return w, inventory, checkpointVerifyError{checkpointVerifyFormatUnsupported}
	}
	if inventory.CheckpointID != id {
		return w, inventory, checkpointVerifyError{checkpointVerifyIdentityMismatch}
	}
	return w, inventory, nil
}

func checkpointVerifyDigest(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

func checkpointVerifyShape(inventory checkpointInventory, witness checkpointWitness, budget *checkpointRetryBudget) error {
	files, directories, total := 0, 0, int64(0)
	previous := ""
	for index, entry := range inventory.Entries {
		if index > 0 && entry.Path <= previous {
			return checkpointVerifyError{checkpointVerifyInventoryMalformed}
		}
		previous = entry.Path
		if !checkpointVerifyPath(entry.Path) {
			return checkpointVerifyError{checkpointVerifyPathRefused}
		}
		switch entry.Type {
		case "file":
			if entry.Size == nil || *entry.Size < 0 || !checkpointVerifyDigest(entry.SHA256) || *entry.Size > math.MaxInt64-total {
				return checkpointVerifyError{checkpointVerifyInventoryMalformed}
			}
			files++
			total += *entry.Size
		case "dir":
			if entry.Size != nil || entry.SHA256 != "" {
				return checkpointVerifyError{checkpointVerifyInventoryMalformed}
			}
			directories++
		default:
			return checkpointVerifyError{checkpointVerifyInventoryMalformed}
		}
	}
	if files != witness.Files || directories != witness.Directories || total != witness.Bytes {
		return checkpointVerifyError{checkpointVerifyCountMismatch}
	}
	budget.limit += time.Duration(files) * checkpointRetryPerFile
	return nil
}

func checkpointVerifyPath(name string) bool {
	if !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00") || !filepath.IsLocal(filepath.FromSlash(name)) ||
		name == checkpointInventoryName || name == checkpointWitnessName {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func checkpointVerifyTree(ctx context.Context, dir string, inventory checkpointInventory) error {
	planned := map[string]bool{checkpointInventoryName: false, checkpointWitnessName: false}
	for _, entry := range inventory.Entries {
		planned[entry.Path] = entry.Type == "dir"
	}
	seen := 0
	err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if name == dir && entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, name)
		directory, known := planned[filepath.ToSlash(rel)]
		if err != nil || !known || directory != entry.IsDir() || (!directory && !entry.Type().IsRegular()) {
			return checkpointVerifyError{checkpointVerifyTreeMismatch}
		}
		seen++
		return nil
	})
	if err != nil || seen != len(planned) {
		return checkpointVerifyReadError(ctx, checkpointVerifyTreeMismatch)
	}
	return nil
}

// checkpointVerifyJSON checks tokens against the existing document types before
// decoding values. Key presence, exact spelling and uniqueness are independent
// of whitespace and object order; omitted optional fields remain distinguishable.
func checkpointVerifyJSON(data []byte, target any) bool {
	if !utf8.Valid(data) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if !checkpointVerifyTokens(decoder, reflect.TypeOf(target).Elem()) {
		return false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return false
	}
	return json.Unmarshal(data, target) == nil
}

func checkpointVerifyTokens(decoder *json.Decoder, typ reflect.Type) bool {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	token, err := decoder.Token()
	if err != nil || token == nil {
		return false
	}
	if typ == reflect.TypeFor[time.Time]() || typ.Kind() == reflect.String {
		_, ok := token.(string)
		return ok
	}
	switch typ.Kind() {
	case reflect.Int, reflect.Int64:
		_, ok := token.(json.Number)
		return ok
	case reflect.Slice:
		if token != json.Delim('[') {
			return false
		}
		for decoder.More() {
			if !checkpointVerifyTokens(decoder, typ.Elem()) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim(']')
	case reflect.Struct:
		if token != json.Delim('{') {
			return false
		}
		fields, required := map[string]reflect.Type{}, map[string]bool{}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name, option, _ := strings.Cut(field.Tag.Get("json"), ",")
			fields[name], required[name] = field.Type, option != "omitempty"
		}
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] || fields[name] == nil || !checkpointVerifyTokens(decoder, fields[name]) {
				return false
			}
			seen[name] = true
		}
		for name, needed := range required {
			if needed && !seen[name] {
				return false
			}
		}
		// A directory carries neither optional file field, including empty strings.
		if typ == reflect.TypeFor[checkpointInventoryEntry]() && seen["sha256"] && !seen["size"] {
			return false
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim('}')
	}
	return false
}
