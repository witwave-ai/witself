//go:build windows

package runtimeconfig

// Lexical validation only: startup configuration must not open the descriptor.
func localShutdownPathValid(path string) bool {
	clean, err := windowsPrivateCleanPath(path)
	return err == nil && clean == path
}
