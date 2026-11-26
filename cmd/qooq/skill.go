package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qooqapp/qooq/pkg/skills"
)

// skillCmd installs the agent skills bundled into this binary. Unlike every
// other command here it never talks to qooqd — it only writes files — so it
// works before a daemon exists, which is exactly when someone installs a
// deploy skill.
func skillCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "skill", Short: "Install the bundled agent skills"}

	cmd.AddCommand(&cobra.Command{
		Use:   "ls",
		Short: "List skills bundled in this binary, and where they are installed",
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Printf("%-16s %-24s %s\n", "NAME", "INSTALLED FOR", "SUMMARY")
			for _, s := range skills.All() {
				fmt.Printf("%-16s %-24s %s\n", s.Name, installedFor(s), s.Summary)
			}
			return nil
		},
	})

	var dir, agent string
	var force bool
	install := &cobra.Command{
		Use:   "install [name]",
		Short: "Install a bundled skill (default: all of them)",
		Long: "Install a bundled skill for a coding agent.\n\n" +
			"Claude Code, Codex and Cursor read the same skill format, so the same\n" +
			"files are installed for each; only the directory differs.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			target := skills.Agent(agent)

			// --dir wins, so an agent this build has never heard of is still
			// reachable; the agent then only decides the closing hint.
			dest := dir
			if dest == "" {
				d, err := skills.DirFor(target)
				if err != nil {
					return err
				}
				dest = d
			}

			want := skills.All()
			if len(args) == 1 {
				s, ok := skills.Lookup(args[0])
				if !ok {
					return fmt.Errorf("unknown skill %q (see: qooq skill ls)", args[0])
				}
				want = []skills.Skill{s}
			}

			for _, s := range want {
				written, err := skills.Install(s, dest, force)
				if err != nil {
					return err
				}
				fmt.Printf("installed %s (%d files) -> %s\n", s.Name, len(written), filepath.Join(dest, s.Name))
			}
			fmt.Printf("\n%s\n", pickUpHint(target))
			return nil
		},
	}
	install.Flags().StringVar(&dir, "dir", "", "skills directory (overrides --agent)")
	install.Flags().StringVar(&agent, "agent", string(skills.DefaultAgent),
		"agent to install for ("+skills.AgentNames()+")")
	install.Flags().BoolVar(&force, "force", false, "overwrite an existing installation")
	cmd.AddCommand(install)

	return cmd
}

// installedFor names the agents that already have this skill.
//
// Listing them beats a yes/no column now that there is more than one place to
// install into: "yes" would be true while the agent in front of the user is
// the one without it.
func installedFor(s skills.Skill) string {
	var found []string
	for _, a := range skills.KnownAgents() {
		dir, err := skills.DirFor(a)
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, s.Name)); err == nil {
			found = append(found, string(a))
		}
	}
	if len(found) == 0 {
		return "-"
	}
	return strings.Join(found, ",")
}

// pickUpHint says what the user has to do for the agent to notice.
//
// Worth the per-agent text: an installed skill that the running session cannot
// see reads as a failed install, and the answer is different for each of them.
func pickUpHint(a skills.Agent) string {
	switch a {
	case skills.AgentClaude:
		return "Restart Claude Code (or start a new session) to pick it up."
	case skills.AgentCodex:
		return "Start a new Codex session to pick it up."
	case skills.AgentCursor:
		return "Reload Cursor (or start a new chat) to pick it up; invoke it with /qooq-deploy."
	case skills.AgentShared:
		return "Installed in the directory Codex and Cursor both scan. Start a new session in either to pick it up."
	}
	return "Start a new agent session to pick it up."
}
