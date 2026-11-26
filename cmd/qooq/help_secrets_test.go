package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// No --help may print a token. Flags whose default came from QOOQ_TOKEN and
// QOOQ_WATCHER_TOKEN printed the values in every usage text, and usage text
// is what gets pasted into chats and bug reports.
func TestHelpNeverPrintsATokenFromTheEnvironment(t *testing.T) {
	t.Setenv("QOOQ_TOKEN", "SENTINEL-QOOQD-TOKEN")
	t.Setenv("QOOQ_WATCHER_TOKEN", "SENTINEL-WATCHER-TOKEN")

	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if u := c.UsageString(); strings.Contains(u, "SENTINEL") {
			t.Errorf("`%s --help` prints a token from the environment", c.CommandPath())
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(newRootCmd())
}

// And the environment is still where the tokens come from.
func TestTokensStillComeFromTheEnvironment(t *testing.T) {
	t.Setenv("QOOQ_TOKEN", "from-env")
	t.Setenv("QOOQ_WATCHER_TOKEN", "watcher-from-env")
	newRootCmd()
	if token != "from-env" || watcherToken != "watcher-from-env" {
		t.Errorf("token=%q watcherToken=%q, want the environment's values", token, watcherToken)
	}
}
