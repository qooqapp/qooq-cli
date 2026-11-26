package main

import (
	"strings"
	"testing"
)

// A qooq Cloud node's settings reach the units through EnvironmentFile=, after the
// generated Environment= lines so systemd lets the file win; without it
// there is nowhere to put them that the next install would not overwrite.
func TestSystemdUnitLoadsTheEnvFile(t *testing.T) {
	u := systemdUnit("/home/q/.qooq", watcherDaemon(), "/usr/local/bin/qooq-watcher", "", "/etc/qooq/node.env")
	i := strings.Index(u, "EnvironmentFile=/etc/qooq/node.env\n")
	if i < 0 {
		t.Fatalf("no EnvironmentFile= line:\n%s", u)
	}
	if j := strings.LastIndex(u, "Environment="+"QOOQ"); j > i {
		t.Errorf("an Environment= line comes after EnvironmentFile=; the file must be last:\n%s", u)
	}
	if strings.Contains(systemdUnit("/home/q/.qooq", watcherDaemon(), "/usr/local/bin/qooq-watcher", "", ""), "EnvironmentFile") {
		t.Error("EnvironmentFile= written without --env-file")
	}
}
