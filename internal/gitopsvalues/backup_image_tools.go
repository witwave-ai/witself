package gitopsvalues

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

type releaseVersion struct{ major, minor, patch uint64 }

func parseReleaseVersion(s string) (releaseVersion, error) {
	invalid := errors.New("value is not a MAJOR.MINOR.PATCH release version")
	if !releaseVersionPattern.MatchString(s) {
		return releaseVersion{}, invalid
	}
	parts := strings.Split(s, ".")
	var values [3]uint64
	for i, part := range parts {
		value, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return releaseVersion{}, invalid
		}
		values[i] = value
	}
	return releaseVersion{major: values[0], minor: values[1], patch: values[2]}, nil
}

func (v releaseVersion) less(o releaseVersion) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	if v.minor != o.minor {
		return v.minor < o.minor
	}
	return v.patch < o.patch
}

type backupImageTools struct{ FirstRelease, Base, AgeX8664, AgeAarch64, AWSCLI string }

const backupImageBaseV0291 = "postgres:18-alpine@sha256:6c538e7206ea40ff740ef27883529390a690b6ead6ba96b44c67a9f7c638e8fd"
const backupImageRepository = "ghcr.io/witwave-ai/images/witself-postgres-backup"
const backupImageDockerfileRel = "images/witself-postgres-backup/Dockerfile"

// This ledger is append-only: a Dockerfile pin change adds an entry in the same
// change. FirstRelease is the first tag that will contain it; confirm it against
// the tags after tagging (docs/release-and-build.md). The same pins mean the same
// age per architecture and aws-cli, not the same image.
var backupImageToolReleases = []backupImageTools{
	{FirstRelease: "0.0.291", Base: backupImageBaseV0291, AgeX8664: "1.3.1-r6", AgeAarch64: "1.3.1-r6", AWSCLI: "2.34.63-r0"},
	{FirstRelease: "0.0.330", Base: backupImageBaseV0291, AgeX8664: "1.3.2-r0", AgeAarch64: "1.3.2-r0", AWSCLI: "2.34.63-r0"},
	{FirstRelease: "0.0.336", Base: backupImageBaseV0291, AgeX8664: "1.3.2-r0", AgeAarch64: "1.3.2-r1", AWSCLI: "2.34.63-r0"},
	{FirstRelease: "0.0.337", Base: backupImageBaseV0291, AgeX8664: "1.3.2-r1", AgeAarch64: "1.3.2-r1", AWSCLI: "2.34.63-r0"},
}

type backupImagePinException struct{ Cell, Tag, Reason string }

type backupImagePolicy struct {
	Ledger         []backupImageTools
	MaxPatchLag    uint64 // 0 turns the release-distance rule off
	CheckBackupOff bool   // also check pins on cells whose backup is off
	Exceptions     []backupImagePinException
}

// Slice 134 merge item 2: a change edits this line and the docs sentence naming it.
const backupImageMaxPatchLag uint64 = 20

// Slice 134 merge item 3: a change edits this line and the docs sentence naming it.
const backupImageCheckBackupOff = true

// A reviewed emergency keeps an older backup image on one cell. Each entry names
// the cell, exact pinned tag, and reason; remove it when that cell is re-pinned at
// a fixed release (docs/backup-and-recovery.md).
var backupImagePinExceptions []backupImagePinException

var defaultBackupImagePolicy = backupImagePolicy{
	Ledger:         backupImageToolReleases,
	MaxPatchLag:    backupImageMaxPatchLag,
	CheckBackupOff: backupImageCheckBackupOff,
	Exceptions:     backupImagePinExceptions,
}

var (
	backupImageBasePattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*(:[A-Za-z0-9_.-]+)?@sha256:[a-f0-9]{64}$`)
	backupImagePackagePattern    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+-r[0-9]+$`)
	backupImageFromPattern       = regexp.MustCompile(`(?m)^FROM (\S+)$`)
	backupImageAPKPattern        = regexp.MustCompile(`(?m)^[\t ]*apk add --no-cache "age=\$age_version" aws-cli=([^ ;]+); \\$`)
	backupImageAPKCommandPattern = regexp.MustCompile(`\bapk add\b`)
	backupImageCasePattern       = regexp.MustCompile(`(?m)^[\t ]*case "\$\(apk --print-arch\)" in \\\n([\s\S]*?)^[\t ]*esac; \\\n[\t ]*apk add --no-cache "age=\$age_version" aws-cli=[^ ;]+; \\$`)
	backupImageArchPattern       = regexp.MustCompile(`^(x86_64|aarch64)\) age_version=([^ ;]+) ;;$`)
	backupImageHexPattern        = regexp.MustCompile(`[0-9a-fA-F]{64}`)
)

// YAML and filesystem errors may quote input values. Keep all guard diagnostics
// free of digest material, including errors from the existing parsers.
func backupImageError(format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	message = backupImageHexPattern.ReplaceAllString(message, "<redacted>")
	return errors.New(strings.ReplaceAll(message, "sha256:", "digest:"))
}

func validateBackupImagePolicy(policy backupImagePolicy) error {
	var problems []error
	if len(policy.Ledger) == 0 {
		problems = append(problems, errors.New("backup image tool ledger must not be empty"))
	}
	var previous releaseVersion
	var previousValid bool
	for i, tools := range policy.Ledger {
		release, err := parseReleaseVersion(tools.FirstRelease)
		if err != nil {
			problems = append(problems, backupImageError("backup image tool ledger entry %d: %v", i+1, err))
		} else if i > 0 && previousValid && !previous.less(release) {
			problems = append(problems, backupImageError("backup image tool ledger entry %d: releases must be strictly ascending", i+1))
		}
		previous, previousValid = release, err == nil
		if !backupImageBasePattern.MatchString(tools.Base) {
			problems = append(problems, backupImageError("backup image tool ledger entry %d: base image must be pinned by digest", i+1))
		}
		for arch, age := range map[string]string{"x86_64": tools.AgeX8664, "aarch64": tools.AgeAarch64} {
			if !backupImagePackagePattern.MatchString(age) {
				problems = append(problems, backupImageError("backup image tool ledger entry %d: age for %s must include a package revision", i+1, arch))
			}
		}
		if !backupImagePackagePattern.MatchString(tools.AWSCLI) {
			problems = append(problems, backupImageError("backup image tool ledger entry %d: aws-cli must include a package revision", i+1))
		}
		if i > 0 && sameBackupImageTools(policy.Ledger[i-1], tools) {
			problems = append(problems, backupImageError("backup image tool ledger entry %d: consecutive tool sets must differ", i+1))
		}
	}
	var first releaseVersion
	var firstValid bool
	if len(policy.Ledger) > 0 {
		var err error
		first, err = parseReleaseVersion(policy.Ledger[0].FirstRelease)
		firstValid = err == nil
	}
	cells := make(map[string]bool)
	for i, exception := range policy.Exceptions {
		if strings.TrimSpace(exception.Cell) == "" {
			problems = append(problems, backupImageError("backup image exception %d: cell must not be empty", i+1))
		}
		if cells[exception.Cell] {
			problems = append(problems, backupImageError("backup image exception %d: duplicate exception for cell %s", i+1, exception.Cell))
		}
		cells[exception.Cell] = true
		tag, err := parseReleaseVersion(exception.Tag)
		if err != nil {
			problems = append(problems, backupImageError("backup image exception %d: tag %v", i+1, err))
		} else if firstValid && tag.less(first) {
			problems = append(problems, backupImageError("backup image exception %d: tag predates the backup image tool ledger", i+1))
		}
		if strings.TrimSpace(exception.Reason) == "" {
			problems = append(problems, backupImageError("backup image exception %d: reason must not be blank", i+1))
		}
	}
	return errors.Join(problems...)
}

func sameBackupImageTools(a, b backupImageTools) bool {
	return a.Base == b.Base && a.AgeX8664 == b.AgeX8664 && a.AgeAarch64 == b.AgeAarch64 && a.AWSCLI == b.AWSCLI
}

func parseBackupImageDockerfile(body []byte) (backupImageTools, error) {
	from := backupImageFromPattern.FindAllSubmatch(body, -1)
	if len(from) != 1 {
		return backupImageTools{}, errors.New("backup image Dockerfile requires exactly one FROM line")
	}
	apk := backupImageAPKPattern.FindAllSubmatch(body, -1)
	if len(apk) != 1 || len(backupImageAPKCommandPattern.FindAll(body, -1)) != 1 {
		return backupImageTools{}, errors.New("backup image Dockerfile requires exactly one apk add command installing age=$age_version and pinned aws-cli")
	}
	archPins, err := parseBackupImageArchitecturePins(body)
	if err != nil {
		return backupImageTools{}, err
	}
	tools := backupImageTools{Base: string(from[0][1]), AgeX8664: archPins["x86_64"], AgeAarch64: archPins["aarch64"], AWSCLI: string(apk[0][1])}
	if !backupImageBasePattern.MatchString(tools.Base) {
		return backupImageTools{}, errors.New("backup image Dockerfile base image must be pinned by digest")
	}
	if !backupImagePackagePattern.MatchString(tools.AgeX8664) || !backupImagePackagePattern.MatchString(tools.AgeAarch64) || !backupImagePackagePattern.MatchString(tools.AWSCLI) {
		return backupImageTools{}, errors.New("backup image Dockerfile age and aws-cli must include package revisions")
	}
	return tools, nil
}

func parseBackupImageArchitecturePins(body []byte) (map[string]string, error) {
	invalid := errors.New("backup image Dockerfile requires exactly x86_64 and aarch64 age_version pins and an unsupported default branch in one apk --print-arch case block")
	blocks := backupImageCasePattern.FindAllSubmatch(body, -1)
	if len(blocks) != 1 {
		return nil, invalid
	}
	lines := strings.Split(strings.TrimSuffix(string(blocks[0][1]), "\n"), "\n")
	if len(lines) != 3 {
		return nil, invalid
	}
	pins := make(map[string]string, 2)
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasSuffix(line, ` \`) {
			return nil, invalid
		}
		line = strings.TrimSuffix(line, ` \`)
		if i == 2 {
			if line != `*) echo "unsupported backup image architecture" >&2; exit 1 ;;` {
				return nil, invalid
			}
			continue
		}
		match := backupImageArchPattern.FindStringSubmatch(line)
		if len(match) != 3 || pins[match[1]] != "" {
			return nil, invalid
		}
		pins[match[1]] = match[2]
	}
	return pins, nil
}

// Compare numeric package components without integer overflow. The policy and
// Dockerfile validators require the MAJOR.MINOR.PATCH-rN format before use.
func backupImagePackageOlder(pin, server string) bool {
	components := func(s string) []string { return strings.Split(strings.ReplaceAll(s, "-r", "."), ".") }
	pinParts, serverParts := components(pin), components(server)
	for i, part := range pinParts {
		left, right := strings.TrimLeft(part, "0"), strings.TrimLeft(serverParts[i], "0")
		if len(left) != len(right) {
			return len(left) < len(right)
		}
		if left != right {
			return left < right
		}
	}
	return false
}

func backupImageToolIndex(ledger []backupImageTools, release releaseVersion) int {
	index := -1
	for i, tools := range ledger {
		first, err := parseReleaseVersion(tools.FirstRelease)
		if err == nil && !release.less(first) {
			index = i
		}
	}
	return index
}

func checkBackupImagePin(policy backupImagePolicy, cell, serverTag string, pins *BackupImagePins) (checked bool, err error) {
	if pins == nil || pins.legacy != "" || pins.Repository != backupImageRepository {
		return false, nil
	}
	if pins.Digest == "" {
		return true, backupImageError("%s: backup image must be pinned by digest", cell)
	}
	pinRelease, err := parseReleaseVersion(pins.Tag)
	if err != nil {
		return true, backupImageError("%s: backup image tag %v", cell, err)
	}
	serverRelease, err := parseReleaseVersion(serverTag)
	if err != nil {
		return true, backupImageError("%s: server tag %v", cell, err)
	}
	pinIndex := backupImageToolIndex(policy.Ledger, pinRelease)
	if pinIndex < 0 {
		return true, backupImageError("%s: backup image pin %s predates the backup image tool ledger", cell, pins.Tag)
	}
	for _, exception := range policy.Exceptions {
		if exception.Cell == cell && exception.Tag == pins.Tag {
			return true, nil
		}
	}
	if !pinRelease.less(serverRelease) {
		return true, nil
	}
	var reasons []string
	serverIndex := backupImageToolIndex(policy.Ledger, serverRelease)
	pinTools, serverTools := policy.Ledger[pinIndex], policy.Ledger[serverIndex]
	if pinTools.Base != serverTools.Base || backupImagePackageOlder(pinTools.AgeX8664, serverTools.AgeX8664) ||
		backupImagePackageOlder(pinTools.AgeAarch64, serverTools.AgeAarch64) || backupImagePackageOlder(pinTools.AWSCLI, serverTools.AWSCLI) {
		reason := fmt.Sprintf("carries age x86_64 %s, aarch64 %s and aws-cli %s; server release %s builds age x86_64 %s, aarch64 %s and aws-cli %s", pinTools.AgeX8664, pinTools.AgeAarch64, pinTools.AWSCLI, serverTag, serverTools.AgeX8664, serverTools.AgeAarch64, serverTools.AWSCLI)
		if pinTools.Base != serverTools.Base {
			reason += " and a different PostgreSQL base"
		}
		reasons = append(reasons, reason)
	}
	if policy.MaxPatchLag > 0 {
		if pinRelease.major != serverRelease.major || pinRelease.minor != serverRelease.minor {
			reasons = append(reasons, fmt.Sprintf("is from an older minor or major release than server release %s", serverTag))
		} else if lag := serverRelease.patch - pinRelease.patch; lag > policy.MaxPatchLag {
			reasons = append(reasons, fmt.Sprintf("is %d patch releases behind server release %s (limit %d)", lag, serverTag, policy.MaxPatchLag))
		}
	}
	if len(reasons) == 0 {
		return true, nil
	}
	return true, backupImageError("%s: backup image pin %s %s; re-pin it in the same change with scripts/roll-cell.sh %s %s --backup-image and the roll's gate arguments (docs/backup-and-recovery.md)", cell, pins.Tag, strings.Join(reasons, "; "), cell, serverTag)
}

func checkBackupImagePinsWith(root string, policy backupImagePolicy) (checked []string, err error) {
	if err := validateBackupImagePolicy(policy); err != nil {
		return nil, err
	}
	var problems []error
	body, err := os.ReadFile(filepath.Join(root, backupImageDockerfileRel))
	if err != nil {
		problems = append(problems, backupImageError("read backup image Dockerfile: %v", err))
	} else if tools, err := parseBackupImageDockerfile(body); err != nil {
		problems = append(problems, err)
	} else if newest := policy.Ledger[len(policy.Ledger)-1]; !sameBackupImageTools(newest, tools) {
		detail := ""
		if newest.Base != tools.Base {
			detail = " (a different base image)"
		}
		problems = append(problems, backupImageError("the Dockerfile pins tools that the newest ledger entry does not record%s; add an entry to internal/gitopsvalues/backup_image_tools.go", detail))
	}
	catalog, err := loadCatalog(root)
	if err != nil {
		problems = append(problems, backupImageError("load backup image cell catalog: %v", err))
		return nil, errors.Join(problems...)
	}
	for _, exception := range policy.Exceptions {
		if _, ok := catalog.Cells[exception.Cell]; !ok {
			problems = append(problems, backupImageError("backup image exception cell %s is not in the cell catalog", exception.Cell))
		}
	}
	cells := make([]string, 0, len(catalog.Cells))
	for cell := range catalog.Cells {
		cells = append(cells, cell)
	}
	sort.Strings(cells)
	for _, cell := range cells {
		body, err := os.ReadFile(filepath.Join(root, cellsDirRel, cell, valuesFileName))
		if err != nil {
			problems = append(problems, backupImageError("%s: read cell values: %v", cell, err))
			continue
		}
		pins, err := parseBackupImagePins(body)
		if err != nil {
			problems = append(problems, backupImageError("%s: %v", cell, err))
			continue
		}
		if pins != nil && pins.legacy == "" && pins.Repository == backupImageRepository && !policy.CheckBackupOff {
			enabled, err := backupImageEnabled(body)
			if err != nil {
				problems = append(problems, backupImageError("%s: parse backup enabled: %v", cell, err))
				continue
			}
			if !enabled {
				continue
			}
		}
		server, parseErr := parseServerPins(body)
		if parseErr != nil {
			problems = append(problems, backupImageError("%s: %v", cell, parseErr))
		}
		cellChecked, checkErr := checkBackupImagePin(policy, cell, server.ImageTag, pins)
		if cellChecked {
			checked = append(checked, cell)
		}
		if checkErr != nil {
			problems = append(problems, checkErr)
		}
	}
	return checked, errors.Join(problems...)
}

func backupImageEnabled(body []byte) (bool, error) {
	var doc struct {
		Apps struct {
			CivoPostgres struct {
				Backup struct {
					Enabled any `yaml:"enabled"`
				} `yaml:"backup"`
			} `yaml:"civoPostgres"`
		} `yaml:"apps"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return false, err
	}
	enabled, _ := doc.Apps.CivoPostgres.Backup.Enabled.(bool)
	return enabled, nil
}

// CheckBackupImagePins refuses release backup pins with older tools or excessive
// release lag relative to their cell's server, subject to the reviewed policy.
// CI calls it from a test only; generation and roll paths do not call this guard.
func CheckBackupImagePins(root string) (checked []string, err error) {
	return checkBackupImagePinsWith(root, defaultBackupImagePolicy)
}
