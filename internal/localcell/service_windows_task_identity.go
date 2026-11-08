package localcell

import "strings"

// Service principals and elevated tokens cannot own an interactive per-user task.
// SID parsing itself belongs to the Windows API (or the strict XML spec parser).
func validWindowsTaskOwner(uid string, elevated bool) bool {
	return !elevated && strings.HasPrefix(uid, "S-1-") &&
		uid != "S-1-5-18" && uid != "S-1-5-19" && uid != "S-1-5-20"
}
