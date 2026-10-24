//go:build windows

package runtimeconfig

import "testing"

func TestLocalSpoolOwnershipAndModes(t *testing.T) {
	t.Skip("LC-S2-PW1: Unix ownership/mode fixture requires Windows DACL coverage")
}
