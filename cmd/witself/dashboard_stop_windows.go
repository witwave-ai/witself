//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/witwave-ai/witself/internal/dashboard"
	"golang.org/x/sys/windows"
)

const dashboardStopRequestName = "the shutdown request"

// Windows cannot send os.Interrupt to an arbitrary process. A named event
// requests the same graceful context cancellation without depending on a
// shared console or interrupting the parent shell. Its name binds the PID and
// exact private registry AccessURL; hashing keeps the URL credential out of
// event metadata. The protected DACL grants only the current user and SYSTEM.
func dashboardStopEventName(entry dashboard.RegistryEntry) (*uint16, error) {
	if entry.PID <= 0 || entry.AccessURL == "" {
		return nil, errors.New("dashboard shutdown identity is unavailable")
	}
	digest := sha256.Sum256([]byte(entry.AccessURL))
	return windows.UTF16PtrFromString(fmt.Sprintf(`Local\witself-dashboard-stop-%d-%x`, entry.PID, digest))
}

func signalDashboardProcess(entry dashboard.RegistryEntry) error {
	name, err := dashboardStopEventName(entry)
	if err != nil {
		return err
	}
	handle, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return fmt.Errorf("open dashboard shutdown event: %w", err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	return windows.SetEvent(handle)
}

func registerDashboardStop(parent context.Context, entry dashboard.RegistryEntry) (context.Context, func(), error) {
	name, err := dashboardStopEventName(entry)
	if err != nil {
		return nil, nil, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, nil, err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, nil, errors.New("current Windows user SID is unavailable")
	}
	sid := user.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sD:P(A;;GA;;;%s)(A;;GA;;;SY)", sid, sid))
	if err != nil {
		return nil, nil, err
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	handle, err := windows.CreateEvent(&attributes, 1, 0, name)
	if err != nil {
		// CreateEvent returns a handle even for ERROR_ALREADY_EXISTS. Never
		// adopt a preexisting event: its ownership and state are unproven.
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return nil, nil, fmt.Errorf("create dashboard shutdown event: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		for ctx.Err() == nil {
			// A bounded wait also makes parent cancellation and cleanup finish
			// promptly without closing a handle still in use by the waiter.
			result, err := windows.WaitForSingleObject(handle, 100)
			if err != nil || result != uint32(windows.WAIT_TIMEOUT) {
				return
			}
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancel()
			<-done
			_ = windows.CloseHandle(handle)
		})
	}, nil
}
