package agenttui

import "context"

// ConsoleState is a value-free lifecycle state for the local web console.
type ConsoleState uint8

const (
	// ConsoleUnavailable means the controller cannot safely determine its state.
	ConsoleUnavailable ConsoleState = iota
	// ConsoleStopped means no console is running for the selected agent.
	ConsoleStopped
	// ConsoleRunning means a verified console is available.
	ConsoleRunning
	// ConsoleConflict means discovery found a console it could not safely adopt.
	ConsoleConflict
)

// ConsoleStatus contains presentation metadata only. Credentials, access URLs,
// registry records, and process handles must stay inside the controller.
type ConsoleStatus struct {
	State ConsoleState
	Owned bool
	Port  int
}

// ConsoleController manages the selected agent's local web console. Open
// starts or reuses a verified console before launching the browser. Stop is an
// explicit user action and may stop a verified reused console. Close stops only
// a child owned by this controller; it must preserve pre-existing consoles.
// Implementations serialize operations, honor cancellation, and return fixed,
// value-free errors. Status must never start a listener or launch a browser.
type ConsoleController interface {
	Status(context.Context) (ConsoleStatus, error)
	Start(context.Context) (ConsoleStatus, error)
	Open(context.Context) (ConsoleStatus, error)
	Stop(context.Context) (ConsoleStatus, error)
	Close() error
}
