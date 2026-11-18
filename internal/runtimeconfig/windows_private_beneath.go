package runtimeconfig

import "strings"

// windowsPrivateBeneathParts splits a name below a held directory at "/" and
// refuses, before any system call, a name with an empty, "." or ".."
// component or one holding a backslash or a colon (an alternate stream).
func windowsPrivateBeneathParts(name string) ([]string, bool) {
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `\:`) {
			return nil, false
		}
	}
	return parts, true
}

// windowsPrivateBeneathDirectory accepts a component before the last only as
// a directory (0x10) that is not a reparse point (0x400), on the held root's
// volume.
func windowsPrivateBeneathDirectory(attributes, volume, rootVolume uint32) bool {
	return attributes&0x10 != 0 && attributes&0x400 == 0 && volume == rootVolume
}
