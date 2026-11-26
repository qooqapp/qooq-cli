package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qooqapp/qooq/pkg/manifest"
)

// initCmd writes a qooq.yaml for the project in front of you.
//
// The detection is the same code the watcher runs on a repo that arrives
// without a manifest — deliberately, because two sets of rules for "what does
// this project look like" would drift, and the one that drifts is always the
// one nobody is watching. What differs is only who is asking: here the person
// running it owns the app and is deciding how it should run, so ports are
// published; there qooq is guessing about a repo it merely found, and
// publishing anything is the operator's call.
//
// Like `skill` and `manifest`, it never talks to a daemon: writing a manifest
// is something you do before there is anything to talk to.
func initCmd() *cobra.Command {
	var (
		app    string
		force  bool
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Write a qooq.yaml for the project in this directory",
		Long: "Look at what is in a project — its Dockerfile, its package.json, what it\n" +
			"exposes — and write a qooq.yaml that describes how to build and run it.\n\n" +
			"The result is a draft to read and correct, not an answer: every guess it\n" +
			"made is written into the file as a comment, so you can see what it based\n" +
			"each line on.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			abs, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
				return fmt.Errorf("%s is not a directory", dir)
			}

			if app == "" {
				app = defaultAppName(abs)
			}

			out := filepath.Join(abs, "qooq.yaml")
			if !dryRun && !force {
				if _, err := os.Stat(out); err == nil {
					return fmt.Errorf("%s already exists — read it first, then pass --force to replace it", out)
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}

			d, err := manifest.Detect(abs, app, "")
			if err != nil {
				return err
			}
			data := manifest.Render(d, manifest.RenderOptions{
				PublishPorts: true,
				Heading:      "# Written by `qooq init` from what is in this directory.\n",
			})

			// Parsing what was just generated is cheap, and a draft that does
			// not survive `manifest validate` would send someone hunting for a
			// mistake in their project that is really a mistake in here.
			if _, err := manifest.Parse(data); err != nil {
				return fmt.Errorf("generated a manifest that does not parse (please report this): %w", err)
			}

			if dryRun {
				_, err := cmd.OutOrStdout().Write(data)
				return err
			}
			if err := os.WriteFile(out, data, 0o644); err != nil {
				return err
			}

			fmt.Printf("wrote %s\n", out)
			fmt.Printf("  detected: %s\n", d.Kind)
			for _, note := range d.Notes {
				fmt.Printf("    - %s\n", note)
			}
			fmt.Printf("\nRead it before deploying — the guesses are marked in the file.\n")
			fmt.Printf("Then: qooq manifest validate %s\n", out)
			return nil
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "app name (default: the directory's name)")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing qooq.yaml")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "print the manifest instead of writing it")
	return cmd
}

// defaultAppName is the directory's name, cleaned into something that can be a
// component name — those have to be DNS labels, since they become part of a
// hostname.
func defaultAppName(dir string) string {
	name := strings.ToLower(filepath.Base(dir))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.', r == ' ':
			b.WriteRune('-')
		}
	}
	cleaned := strings.Trim(b.String(), "-")
	for strings.Contains(cleaned, "--") {
		cleaned = strings.ReplaceAll(cleaned, "--", "-")
	}
	if cleaned == "" {
		return "app"
	}
	return cleaned
}
