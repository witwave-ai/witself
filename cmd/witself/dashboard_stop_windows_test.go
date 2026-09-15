//go:build windows

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/dashboard"
	"golang.org/x/sys/windows"
)

func windowsDashboardStopEntry() dashboard.RegistryEntry {
	return dashboard.RegistryEntry{
		PID:       os.Getpid(),
		AccessURL: "http://127.0.0.1:54321/?token=" + rand.Text(),
	}
}

func TestDashboardWindowsStopEventCancelsOnlyExactIdentity(t *testing.T) {
	entry := windowsDashboardStopEntry()
	ctx, release, err := registerDashboardStop(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	for _, other := range []dashboard.RegistryEntry{
		{PID: entry.PID + 1, AccessURL: entry.AccessURL},
		{PID: entry.PID, AccessURL: entry.AccessURL + "different"},
	} {
		if err := signalDashboardProcess(other); err == nil {
			t.Fatal("shutdown accepted a different PID or access URL")
		}
	}
	if ctx.Err() != nil {
		t.Fatal("wrong identity canceled the dashboard")
	}
	// Real Windows signaling runs in the test process, without a console or
	// process group. The prior Process.Signal(SIGINT) returned EWINDOWS here.
	if err := signalDashboardProcess(entry); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("dashboard context was not canceled by its shutdown event")
	}
	release()
	if err := signalDashboardProcess(entry); err == nil {
		t.Fatal("released dashboard shutdown event remains open")
	}
}

func TestDashboardWindowsStopEventRefusesPreexistingEvent(t *testing.T) {
	entry := windowsDashboardStopEntry()
	ctx, release, err := registerDashboardStop(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, cleanup, err := registerDashboardStop(context.Background(), entry); err == nil {
		cleanup()
		t.Fatal("adopted a preexisting shutdown event")
	} else if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		t.Fatalf("unexpected duplicate-event error: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("rejecting a duplicate disturbed the original dashboard")
	}
	if err := signalDashboardProcess(entry); err != nil {
		t.Fatalf("duplicate rejection closed the original event: %v", err)
	}
}

func TestDashboardWindowsStopEventProtectsOwnerAndCleansUpOnCancellation(t *testing.T) {
	entry := windowsDashboardStopEntry()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, release, err := registerDashboardStop(parent, entry)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	name, err := dashboardStopEventName(entry)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.OpenEvent(windows.READ_CONTROL, false, name)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	_ = windows.CloseHandle(handle)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("dashboard shutdown event DACL is not protected")
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if owner == nil || !owner.Equals(user.User.Sid) {
		t.Fatal("dashboard shutdown event is not owned by the current user")
	}
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("parent cancellation did not reach dashboard context")
	}
	release()
	if err := signalDashboardProcess(entry); err == nil {
		t.Fatal("parent cancellation left the shutdown event open after cleanup")
	}
}
