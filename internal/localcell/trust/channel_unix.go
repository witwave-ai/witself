//go:build !windows

package trust

import (
	"os"
	"strconv"
	"syscall"
)

func validChannelIdentity(identity string) bool {
	value, err := strconv.ParseUint(identity, 10, 32)
	return err == nil && strconv.FormatUint(value, 10) == identity
}

func channelIdentityMatches(_ string, info os.FileInfo, identity string) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && strconv.FormatUint(uint64(stat.Uid), 10) == identity
}

func channelFileIdentity(_ string, info os.FileInfo) (HostIdentity, error) {
	return hostFileIdentity(nil, info)
}
