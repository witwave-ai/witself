//go:build windows

package runtimeconfig

import "testing"

func TestLocalSpoolOwnershipAndModes(t *testing.T) {
	t.Skip("Unix ownership/mode fixture does not apply on Windows; Windows uses the current token owner")
}
