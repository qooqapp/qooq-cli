package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// logsCmd streams a component's container output. Without -f it prints the
// (optionally tailed) output once and exits; with -f it keeps streaming
// until Ctrl-C (SIGINT/SIGTERM) or the server ends the stream. The Ctrl-C
// case is deliberately not surfaced as an error: it is the expected way to
// stop a follow session.
func logsCmd() *cobra.Command {
	var follow bool
	var tail int
	var timestamps bool
	cmd := &cobra.Command{
		Use:   "logs <id-or-name>",
		Short: "Show a component's container logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := resolve(cmd, args[0])
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			if follow {
				sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
				defer stop()
				ctx = sigCtx
			}

			rc, err := newClient().Logs(ctx, c.ID, follow, tail, timestamps)
			if err != nil {
				return err
			}
			defer rc.Close()

			if timestamps {
				err = copySimplifyingTimestamps(os.Stdout, rc)
			} else {
				_, err = io.Copy(os.Stdout, rc)
			}
			if err != nil && ctx.Err() != nil {
				// Stream aborted because we cancelled ctx (Ctrl-C during
				// follow): that is a normal way to end `logs -f`, not a
				// failure to report.
				return nil
			}
			if err != nil {
				return fmt.Errorf("stream logs: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow log output")
	cmd.Flags().IntVar(&tail, "tail", 0, "show only the last N lines (0 = all)")
	cmd.Flags().BoolVarP(&timestamps, "timestamps", "t", false, "prefix each line with its UTC date-time, e.g. 2026-07-16 06:59:14 (docker runtime only)")
	return cmd
}

// copySimplifyingTimestamps rewrites the RFC3339Nano prefix docker puts on
// every line (when the timestamps option is set) into the CLI's uniform
// "2006-01-02 15:04:05" UTC shape — second precision is enough for reading
// logs, and it matches every other time this CLI prints. Lines whose first
// token doesn't parse as a timestamp pass through untouched.
func copySimplifyingTimestamps(w io.Writer, r io.Reader) error {
	sc := bufio.NewScanner(r)
	// Log lines can exceed bufio's 64KB default (minified JS, stack dumps).
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, ' '); i > 0 {
			if t, err := time.Parse(time.RFC3339Nano, line[:i]); err == nil {
				line = formatUTC(t) + line[i:]
			}
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return sc.Err()
}
