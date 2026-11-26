// Command qooq is the CLI client for the qooq daemon. It talks to qooqd only
// through pkg/client (the REST SDK); it never touches Docker or the store.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/qooqapp/qooq/pkg/client"
)

// defaultAddr is the daemon base URL; overridable with --addr or QOOQ_ADDR.
const defaultAddr = "http://127.0.0.1:7777"

var addr, token string

func newClient() *client.Client { return client.New(addr, client.WithToken(token)) }

// version is injected at build time with -ldflags -X main.version (see the
// Makefile and .goreleaser.yaml). "dev" is the honest answer for a binary
// built straight from a working tree.
//
// It is declared here rather than shared with the qooq module because
// internal/ packages cannot cross a module boundary — and this binary is
// released on its own schedule anyway, so its version is genuinely its own.
var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "qooq",
		Short:         "qooq — Component-as-a-Service CLI",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	envAddr := os.Getenv("QOOQ_ADDR")
	if envAddr == "" {
		envAddr = defaultAddr
	}
	root.PersistentFlags().StringVar(&addr, "addr", envAddr, "qooqd base URL")
	root.PersistentFlags().StringVar(&token, "token", "", "bearer token for qooqd API (default: QOOQ_TOKEN env)")
	// The env value goes into the variable, not the flag's default: cobra
	// prints a flag's default in every --help, and a default read from the
	// environment printed the token itself. --token still overrides this
	// when the flags are parsed.
	token = os.Getenv("QOOQ_TOKEN")

	root.AddCommand(upCmd(), downCmd(), statusCmd(), systemCmd(), componentCmd(), groupCmd(), imageCmd(), watchCmd(), stackCmd(), composeCmd(), skillCmd(), serviceCmd(), manifestCmd(), initCmd(), authCmd(), projectCmd(), deployDirCmd(), secretCmd(), configCmd(), mcpCmd())
	return root
}
