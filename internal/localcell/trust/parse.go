package trust

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var unsignedInteger = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
var releaseTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var glibcVersion = regexp.MustCompile(`^2\.[0-9]{2}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var timestampPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)

// StrictProfile checks the common JSON wire profile without interpreting a schema.
// Verifiers call it only after authenticating the exact raw document bytes.
func StrictProfile(raw []byte) error {
	_, err := profileObject(raw)
	return err
}

func profileObject(raw []byte) (map[string]any, error) {
	if !utf8.Valid(raw) || !validSurrogates(raw) {
		return nil, ReasonProfileViolation
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	value, err := profileValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, ReasonInvalidJSON
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, ReasonProfileViolation
	}
	return obj, nil
}

func profileValue(dec *json.Decoder, depth int) (any, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, ReasonInvalidJSON
	}
	switch v := token.(type) {
	case json.Delim:
		if depth >= 8 {
			return nil, ReasonProfileViolation
		}
		switch v {
		case '{':
			obj := make(map[string]any)
			for dec.More() {
				name, err := dec.Token()
				if err != nil {
					return nil, ReasonInvalidJSON
				}
				key, ok := name.(string)
				if !ok {
					return nil, ReasonInvalidJSON
				}
				if len(key) > 4096 {
					return nil, ReasonProfileViolation
				}
				if _, found := obj[key]; found {
					return nil, ReasonDuplicateKey
				}
				val, err := profileValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				obj[key] = val
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim('}') {
				return nil, ReasonInvalidJSON
			}
			return obj, nil
		case '[':
			array := []any{}
			for dec.More() {
				val, err := profileValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				array = append(array, val)
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim(']') {
				return nil, ReasonInvalidJSON
			}
			return array, nil
		default:
			return nil, ReasonInvalidJSON
		}
	case string:
		if len(v) > 4096 {
			return nil, ReasonProfileViolation
		}
	}
	return token, nil
}

// encoding/json replaces unpaired UTF-16 escapes; the wire profile refuses them.
func validSurrogates(raw []byte) bool {
	inString := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		n, e := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if e != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n < 0xd800 || n > 0xdbff {
			continue
		}
		if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		n, e = strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if e != nil || n < 0xdc00 || n > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}

func parseDocument(raw []byte, maxBytes int, out any, topUnknown bool) error {
	if len(raw) > maxBytes {
		if maxBytes == MaxEnvelopeBytes {
			return ReasonEnvelopeTooLarge
		}
		return ReasonDocumentTooLarge
	}
	obj, err := profileObject(raw)
	if err != nil {
		return err
	}
	typ := reflect.TypeOf(out).Elem()
	if err = checkShape(obj, typ, topUnknown); err != nil {
		return err
	}
	if topUnknown {
		understood := fieldTypes(typ)
		critical, ok := obj["critical"].([]any)
		if !ok {
			return ReasonInvalidDocument
		}
		seen := map[string]bool{}
		for _, entry := range critical {
			name, ok := entry.(string)
			if !ok {
				return ReasonInvalidDocument
			}
			if _, ok = understood[name]; !ok {
				return ReasonUnknownCritical
			}
			if seen[name] {
				return ReasonInvalidDocument
			}
			seen[name] = true
		}
	}
	// Decode only exact top-level field names. encoding/json's struct decoder
	// accepts case-insensitive aliases, which would let an ignored unknown field
	// overwrite a previously checked known field (including a nested schema).
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return ReasonInvalidDocument
	}
	destination := reflect.ValueOf(out).Elem()
	destination.Set(reflect.Zero(typ))
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if value, exists := fields[name]; exists {
			if err = json.Unmarshal(value, destination.Field(i).Addr().Interface()); err != nil {
				return ReasonInvalidDocument
			}
		}
	}
	return nil
}
func fieldTypes(t reflect.Type) map[string]reflect.StructField {
	fields := map[string]reflect.StructField{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fields[strings.Split(f.Tag.Get("json"), ",")[0]] = f
	}
	return fields
}
func checkShape(value any, t reflect.Type, allowUnknown bool) error {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := value.(map[string]any)
		if !ok {
			return ReasonInvalidDocument
		}
		fields := fieldTypes(t)
		for name, v := range obj {
			f, ok := fields[name]
			if !ok {
				if allowUnknown {
					continue
				}
				return ReasonUnknownField
			}
			if err := checkShape(v, f.Type, false); err != nil {
				return err
			}
		}
		for name, f := range fields {
			if _, ok := obj[name]; !ok && !strings.Contains(f.Tag.Get("json"), ",omitempty") {
				return ReasonInvalidDocument
			}
		}
	case reflect.Slice:
		arr, ok := value.([]any)
		if !ok {
			return ReasonInvalidDocument
		}
		for _, v := range arr {
			if err := checkShape(v, t.Elem(), false); err != nil {
				return err
			}
		}
	case reflect.String:
		if _, ok := value.(string); !ok {
			return ReasonInvalidDocument
		}
	case reflect.Bool:
		if _, ok := value.(bool); !ok {
			return ReasonInvalidDocument
		}
	case reflect.Uint64, reflect.Int:
		number, ok := value.(json.Number)
		if !ok || !unsignedInteger.MatchString(string(number)) {
			return ReasonProfileViolation
		}
		if _, err := strconv.ParseUint(string(number), 10, 64); err != nil {
			return ReasonProfileViolation
		}
	default:
		return ReasonInvalidDocument
	}
	return nil
}
func validateCommon(version int, kind, want, issued, expires string) error {
	if version != 1 {
		return ReasonInvalidDocument
	}
	if kind != want {
		return ReasonWrongKind
	}
	if !timestampPattern.MatchString(issued) || !timestampPattern.MatchString(expires) {
		return ReasonInvalidDocument
	}
	start, e := time.Parse(time.RFC3339, issued)
	end, e2 := time.Parse(time.RFC3339, expires)
	if e != nil || e2 != nil || !end.After(start) {
		return ReasonInvalidDocument
	}
	return nil
}

// ParseComponentSet checks the strict component-set schema; it does not authenticate bytes.
func ParseComponentSet(raw []byte) (ComponentSet, error) {
	var doc ComponentSet
	if err := parseDocument(raw, MaxComponentSetBytes, &doc, true); err != nil {
		return doc, err
	}
	if err := validateCommon(doc.FormatVersion, doc.Kind, "component_set", doc.IssuedAt, doc.ExpiresAt); err != nil {
		return doc, err
	}
	if doc.RootSequence == 0 || doc.DelegationSequence == 0 || doc.ReleaseSequence == 0 || doc.Channel != "stable" {
		return doc, ReasonInvalidDocument
	}
	if !releaseTag.MatchString(doc.ReleaseTag) {
		return doc, ReasonWrongTag
	}
	if doc.Source.Repository == "" || !commitPattern.MatchString(doc.Source.Commit) || !digestPattern.MatchString(doc.Provenance.ChecksumsSHA256) || !ValidPath(doc.Provenance.SigstoreBundle) || !ValidPath(doc.License) || !ValidPath(doc.SBOM) {
		return doc, ReasonInvalidDocument
	}
	seenTargets := map[uint64]bool{}
	for _, target := range doc.Rollback.AllowedTargets {
		if target == 0 || target >= doc.ReleaseSequence || seenTargets[target] {
			return doc, ReasonInvalidDocument
		}
		seenTargets[target] = true
	}
	if len(doc.Platforms) == 0 || len(doc.Platforms) > 6 {
		return doc, ReasonWrongPlatform
	}
	seenPlatforms := map[string]bool{}
	for _, platform := range doc.Platforms {
		if err := validatePlatform(platform); err != nil {
			return doc, err
		}
		key := platform.OS + "/" + platform.Arch
		if seenPlatforms[key] {
			return doc, ReasonWrongPlatform
		}
		seenPlatforms[key] = true
	}
	digest := sha256.Sum256(raw)
	for i := range doc.Platforms {
		for j := range doc.Platforms[i].Components {
			doc.Platforms[i].Components[j].manifestDigest = hex.EncodeToString(digest[:])
		}
	}
	return doc, nil
}
func validatePlatform(p Platform) error {
	if (p.OS != "linux" && p.OS != "darwin" && p.OS != "windows") || (p.Arch != "arm64" && p.Arch != "amd64") || p.MinOS == "" {
		return ReasonWrongPlatform
	}
	if (p.OS == "linux" && !glibcVersion.MatchString(p.GlibcFloor)) || (p.OS != "linux" && p.GlibcFloor != "") {
		return ReasonWrongPlatform
	}
	roles := map[string]int{}
	paths := map[string]bool{}
	archives := map[string]bool{}
	for _, c := range p.Components {
		if err := ValidateComponent(c); err != nil {
			return err
		}
		roles[c.Role]++
		if archives[c.Archive.Name] {
			return ReasonInventory
		}
		archives[c.Archive.Name] = true
		for _, entry := range c.Inventory {
			if paths[entry.Path] {
				return ReasonInventory
			}
			paths[entry.Path] = true
		}
	}
	for _, role := range []string{"app", "service_host", "server", "postgresql"} {
		if roles[role] != 1 {
			return ReasonRoles
		}
	}
	if roles["worker"] < 1 || roles["extension"] > 1 {
		return ReasonRoles
	}
	for _, c := range p.Components {
		seen := map[string]bool{}
		for _, dep := range c.DependsOn {
			if roles[dep] == 0 || dep == c.Role || seen[dep] {
				return ReasonRoles
			}
			seen[dep] = true
		}
	}
	// Dependencies are role-level and must be acyclic.
	graph := map[string][]string{}
	for _, c := range p.Components {
		graph[c.Role] = append(graph[c.Role], c.DependsOn...)
	}
	active, done := map[string]bool{}, map[string]bool{}
	var visit func(string) bool
	visit = func(role string) bool {
		if active[role] {
			return false
		}
		if done[role] {
			return true
		}
		active[role] = true
		for _, dep := range graph[role] {
			if !visit(dep) {
				return false
			}
		}
		active[role] = false
		done[role] = true
		return true
	}
	for role := range roles {
		if !visit(role) {
			return ReasonRoles
		}
	}
	return nil
}

// ValidateComponent validates the bounded declaration before archive access.
func ValidateComponent(c Component) error {
	switch c.Role {
	case "app", "service_host", "server", "worker", "postgresql", "extension":
	default:
		return ReasonRoles
	}
	if c.Version == "" {
		return ReasonInvalidDocument
	}
	if !ValidPath(c.Archive.Name) || strings.Contains(c.Archive.Name, "/") || c.Archive.Size == 0 || c.Archive.Size > MaxArchiveBytes || !digestPattern.MatchString(c.Archive.SHA256) || (c.Archive.Format != "tar.gz" && c.Archive.Format != "zip") {
		return ReasonInventory
	}
	if len(c.Inventory) == 0 || len(c.Inventory) > MaxInventoryEntries {
		return ReasonInventory
	}
	var total uint64
	paths := map[string]bool{}
	for _, entry := range c.Inventory {
		if !ValidPath(entry.Path) {
			return ReasonPath
		}
		if paths[entry.Path] {
			return ReasonInventory
		}
		paths[entry.Path] = true
		if entry.Mode != "0644" && entry.Mode != "0755" {
			return ReasonMode
		}
		if entry.Executable != (entry.Mode == "0755") {
			return ReasonMode
		}
		if !digestPattern.MatchString(entry.SHA256) || entry.Size > c.Archive.Size-total {
			return ReasonInventory
		}
		total += entry.Size
	}
	for path := range paths {
		parts := strings.Split(path, "/")
		for i := 1; i < len(parts); i++ {
			if paths[strings.Join(parts[:i], "/")] {
				return ReasonInventory
			}
		}
	}
	if c.Schema.AcceptedMin > c.Schema.Target || c.Schema.Target > c.Schema.AcceptedMax {
		return ReasonSchema
	}
	if len(c.RequiredExtensions) != 0 {
		return ReasonExtensions
	}
	if c.Role == "postgresql" {
		pg := c.PostgreSQL
		if pg == nil || pg.Major != 16 || pg.Version != c.Version || !strings.HasPrefix(pg.Version, "16.") || pg.Distribution == "" || pg.Build == "" || pg.DataLayoutVersion == 0 || pg.RuntimeLibraries == nil {
			return ReasonPostgreSQL
		}
		for _, lib := range pg.RuntimeLibraries {
			if !ValidPath(lib) {
				return ReasonPath
			}
		}
	} else if c.PostgreSQL != nil {
		return ReasonPostgreSQL
	}
	owned := map[string]bool{}
	for _, list := range [][]string{c.Ownership.PackageOwnedPaths, c.Ownership.PrivatePayloadPaths} {
		for _, path := range list {
			if !ValidPath(path) {
				return ReasonPath
			}
			if owned[path] || !paths[path] {
				return ReasonInventory
			}
			owned[path] = true
		}
	}
	if len(owned) != len(paths) {
		return ReasonInventory
	}
	return nil
}

// ParseDelegation checks the strict delegation schema; it does not authenticate bytes.
func ParseDelegation(raw []byte) (Delegation, error) {
	var d Delegation
	if err := parseDocument(raw, MaxAuthorityBytes, &d, true); err != nil {
		return d, err
	}
	if err := validateCommon(d.FormatVersion, d.Kind, "delegation", d.IssuedAt, d.ExpiresAt); err != nil {
		return d, err
	}
	if d.RootSequence == 0 || d.DelegationSequence == 0 || len(d.Keys) == 0 {
		return d, ReasonInvalidDocument
	}
	keys := map[string]bool{}
	for _, key := range d.Keys {
		if _, err := ParsePublicKey(key); err != nil {
			return d, err
		}
		if keys[key.KeyID] {
			return d, ReasonInvalidKey
		}
		keys[key.KeyID] = true
	}
	revoked := map[string]bool{}
	for _, key := range d.RevokedKeyIDs {
		if !digestPattern.MatchString(key) || revoked[key] {
			return d, ReasonInvalidKey
		}
		revoked[key] = true
	}
	return d, nil
}

// ParseRootRotation checks the strict root-rotation schema; it does not authenticate bytes.
func ParseRootRotation(raw []byte) (RootRotation, error) {
	var d RootRotation
	if err := parseDocument(raw, MaxAuthorityBytes, &d, true); err != nil {
		return d, err
	}
	if err := validateCommon(d.FormatVersion, d.Kind, "root_rotation", d.IssuedAt, d.ExpiresAt); err != nil {
		return d, err
	}
	if d.RootSequence == 0 || d.RootSequence == ^uint64(0) || d.NewSequence != d.RootSequence+1 {
		return d, ReasonSequence
	}
	if _, err := ParsePublicKey(d.NewRoot); err != nil {
		return d, err
	}
	return d, nil
}
