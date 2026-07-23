//go:build !windows

package transcriptcapture

// Tests render Windows hook configuration on non-Windows hosts. Production
// Windows builds resolve the actual system directory from the kernel instead.
func codexWindowsPowerShellExecutable() (string, error) {
	return `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, nil
}
