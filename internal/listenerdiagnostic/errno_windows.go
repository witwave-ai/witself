package listenerdiagnostic

import "golang.org/x/sys/windows"

// ErrAddressInUse is the Winsock error, also used by hermetic fixtures.
const ErrAddressInUse = windows.WSAEADDRINUSE
