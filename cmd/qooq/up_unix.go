//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach puts the daemon in its own session so it survives the terminal that
// started it. Without this, closing the window would take the daemons with it
// — which is the whole difference between "started in the background" and
// "started until you look away".
func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
