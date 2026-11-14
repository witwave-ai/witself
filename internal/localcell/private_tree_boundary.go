package localcell

import (
	"errors"
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
	// privateTreeAlias is a direct-child case alias of recovery, refused before
	// either validator can open it or a walk can descend into it.
	privateTreeAlias
)

// classifyPrivateTreePath compares path byte for byte with the paths a walk of
// root reports, without normalisation. Only exactly <root>/recovery is the
// boundary; same-byte-length EqualFold aliases of that direct child are refused.
// The root itself and every other path keep their PGDATA or strict rule.
func classifyPrivateTreePath(root, path string) privateTreeClass {
	boundary := filepath.Join(root, "recovery")
	if path == boundary {
		return privateTreeBoundary
	}
	prefix, name := filepath.Split(boundary)
	if len(path) == len(boundary) && strings.HasPrefix(path, prefix) && strings.EqualFold(path[len(prefix):], name) {
		return privateTreeAlias
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
// validates anything below it. Every other boundary object returns the strict
// result unchanged: fs.SkipDir for a
// non-directory would make the walk skip the rest of the cell root. An alias
// refuses immediately, without either validator or fs.SkipDir.
func privateTreeEntry(root, path string, directory bool, strict func() error, postgres func(pg string) error) error {
	switch classifyPrivateTreePath(root, path) {
	case privateTreeAlias:
		return errors.New("cannot inspect private local cell layout")
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
