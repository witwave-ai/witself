package localcell

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// privateTreeClass is the rule an ordinary whole-root private-tree walk
// applies to one path it reports.
type privateTreeClass int

const (
	// privateTreeStrict is the strict private-object rule.
	privateTreeStrict privateTreeClass = iota
	// privateTreePostgres is the PGDATA rule for <root>/pg and everything
	// below it.
	privateTreePostgres
	// privateTreeBoundary is <root>/recovery. An ordinary walk validates that
	// one object with the strict rule and never descends below it; content
	// below it is validated only by the operations that use it.
	privateTreeBoundary
)

// classifyPrivateTreePath compares path byte for byte with the paths a walk of
// root reports. Only exactly <root>/recovery is the boundary: no prefix,
// base-name or case-folded spelling qualifies and the root itself never does,
// so every other path keeps its PGDATA or strict rule and is walked in full.
func classifyPrivateTreePath(root, path string) privateTreeClass {
	if path == filepath.Join(root, "recovery") {
		return privateTreeBoundary
	}
	pg := filepath.Join(root, "pg")
	if path == pg || strings.HasPrefix(path, pg+string(filepath.Separator)) {
		return privateTreePostgres
	}
	return privateTreeStrict
}

// privateTreeEntry applies the whole-root walk rule to one reported entry.
// strict validates the entry with the strict rule; postgres validates it with
// the PGDATA rule rooted at pg. Only a boundary directory whose own strict
// validation passed answers fs.SkipDir, so the walk neither lists nor
// validates anything below it. Every other boundary object, like every
// refusal, returns the strict result unchanged: fs.SkipDir for a
// non-directory would make the walk skip the rest of the cell root.
func privateTreeEntry(root, path string, directory bool, strict func() error, postgres func(pg string) error) error {
	switch classifyPrivateTreePath(root, path) {
	case privateTreeBoundary:
		if err := strict(); err != nil || !directory {
			return err
		}
		return fs.SkipDir
	case privateTreePostgres:
		return postgres(filepath.Join(root, "pg"))
	default:
		return strict()
	}
}
