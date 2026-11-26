//go:build windows

package main

import "os/exec"

// detach is a no-op on Windows, which has no session concept to detach from.
//
// The daemons do not support Windows anyway (qooqd's process runtime refuses
// to start there), so this exists to keep the CLI compiling for the platform
// rather than to make `qooq up` work on it.
func detach(c *exec.Cmd) {}
