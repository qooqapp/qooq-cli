package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func systemCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "system", Short: "Daemon health and version"}

	cmd.AddCommand(&cobra.Command{
		Use:   "health",
		Short: "Check daemon health",
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := newClient().Health(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Printf("status:  %s\nruntime: %s\n", h.Status, h.Runtime)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Show daemon version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			v, err := newClient().Version(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Printf("version:     %s\nruntime:     %s\napi_version: %s\n", v.Version, v.Runtime, v.APIVersion)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "backup <output-file>",
		Short: "Write a consistent snapshot of the daemon datastore to a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 0600: the snapshot may contain sensitive data; don't leave it world-readable.
			f, err := os.OpenFile(args[0], os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			defer f.Close()
			if err := newClient().Backup(cmd.Context(), f); err != nil {
				// Don't leave a misleading partial/empty file behind.
				os.Remove(args[0])
				return err
			}
			fmt.Printf("backup written to %s\n", args[0])
			return nil
		},
	})

	return cmd
}
