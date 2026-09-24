//go:build !darwin && !linux

package clientinventory

import "context"

// Platforms without the descriptor-relative no-follow implementation fail closed.
type directory struct{}

func openDirectory(string) (*directory, error)           { return nil, ErrUnsupportedPlatform }
func (*directory) close()                                {}
func (*directory) regular(context.Context, string) error { return ErrUnsupportedPlatform }
func (*directory) read(context.Context, string, int64) ([]byte, error) {
	return nil, ErrUnsupportedPlatform
}
