//go:build windows

package localcell

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// NtQuerySystemInformation function (winternl.h) explicitly has a
// compatibility-sensitive contract. The ABI premise is phnt ntexapi.h,
// SYSTEM_BOOT_ENVIRONMENT_INFORMATION (public, non-SDK), revision 53fbbdc5b5d2:
// https://github.com/winsiderss/phnt/blob/53fbbdc5b5d2/ntexapi.h
// SystemBootEnvironmentInformation is class 90; BootIdentifier is at 0 (GUID,
// 16 bytes), FirmwareType at 16 (4-byte FIRMWARE_TYPE), and BootFlags at 24
// (ULONGLONG), total 32, with four padding bytes at 20. No Microsoft SDK/WDK
// definition exists for this class. The first Windows CI row's value-free
// service_boot_identity line remains the executable oracle for this premise.
// No time, uptime, shared-data page, or alternate query is a fallback authority.
type windowsBootEnvironmentInformation struct {
	BootIdentifier windows.GUID
	FirmwareType   uint32
	_              [4]byte
	BootFlags      uint64
}

const windowsBootEnvironmentSize = 32

var windowsQueryBootEnvironment = queryWindowsBootEnvironment

var windowsNtQuerySystemInformation = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQuerySystemInformation")

func queryWindowsBootEnvironment() (uint32, uint32, []byte) {
	data := make([]byte, windowsBootEnvironmentSize)
	if windowsNtQuerySystemInformation.Find() != nil {
		return 0xc0000002, 0, data // STATUS_NOT_IMPLEMENTED; value-free diagnostic.
	}
	var length uint32
	status, _, _ := windowsNtQuerySystemInformation.Call(
		uintptr(windows.SystemBootEnvironmentInformation),
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)),
		uintptr(unsafe.Pointer(&length)),
	)
	runtime.KeepAlive(data)
	return uint32(status), length, data
}

func windowsBootGUIDNonzero(data []byte) bool {
	if len(data) < 16 {
		return false
	}
	for _, b := range data[:16] {
		if b != 0 {
			return true
		}
	}
	return false
}

func serviceBootID() (string, error) {
	status, length, data := windowsQueryBootEnvironment()
	if status != 0 || length < 16 || length > windowsBootEnvironmentSize || int(length) > len(data) || len(data) > windowsBootEnvironmentSize || !windowsBootGUIDNonzero(data) {
		return "", errors.New("service_boot_identity_unavailable")
	}
	return fmt.Sprintf("windows:%08x-%04x-%04x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		binary.LittleEndian.Uint32(data[:4]), binary.LittleEndian.Uint16(data[4:6]), binary.LittleEndian.Uint16(data[6:8]),
		data[8], data[9], data[10], data[11], data[12], data[13], data[14], data[15]), nil
}

func windowsBootEnvironmentDiagnostic() string {
	status, length, data := windowsQueryBootEnvironment()
	return fmt.Sprintf("service_boot_identity ntstatus=0x%08x length=%d guid_nonzero=%t", status, length, windowsBootGUIDNonzero(data))
}
