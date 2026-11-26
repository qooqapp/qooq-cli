package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/qooqapp/qooq/pkg/watcherclient"
)

// stackCmd is the `qooq stack` subtree: deploy several projects together from
// one qooq.stack.yaml.
//
// It is a sibling of `qooq watch`, not a child, because the two are different
// units of work: a target is one project, a stack is a group of them with an
// order. They share the watcher address flags (both talk to qooq-watcher) and
// nothing else.
func stackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stack",
		Short: "Deploy a group of projects together from one qooq.stack.yaml",
		Long: "Deploy a group of projects together from one qooq.stack.yaml.\n\n" +
			"A stack is one deployed instance of that manifest: each project in it\n" +
			"becomes a component in a namespace and network named after the stack, so\n" +
			"projects address each other by their short names and two stacks made from\n" +
			"the same manifest never collide.",
	}

	envAddr := os.Getenv("QOOQ_WATCHER_ADDR")
	if envAddr == "" {
		envAddr = defaultWatcherAddr
	}
	cmd.PersistentFlags().StringVar(&watcherAddr, "watcher-addr", envAddr, "qooq-watcher base URL (default: QOOQ_WATCHER_ADDR env)")
	cmd.PersistentFlags().StringVar(&watcherToken, "watcher-token", "", "bearer token for qooq-watcher API (default: QOOQ_WATCHER_TOKEN env)")
	watcherToken = os.Getenv("QOOQ_WATCHER_TOKEN") // not the flag default: --help prints that (see main.go)

	cmd.AddCommand(stackAddCmd(), stackLsCmd(), stackGetCmd(), stackRmCmd(), stackTriggerCmd(), stackResumeCmd(), stackManifestCmd())
	return cmd
}

func stackAddCmd() *cobra.Command {
	var (
		name, repo, branch        string
		authToken, authSSHKeyFile string
		manifestPath              string
		poll                      int
		allowDuplicate            bool
	)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a stack from a repository holding a qooq.stack.yaml",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var err error
			if authToken, err = atFileValue(authToken); err != nil {
				return fmt.Errorf("--auth-token: %w", err)
			}
			if authToken != "" && authSSHKeyFile != "" {
				return fmt.Errorf("--auth-token and --auth-ssh-key are mutually exclusive")
			}
			var authType, authCredential string
			switch {
			case authToken != "":
				authType, authCredential = "token", authToken
			case authSSHKeyFile != "":
				pem, err := os.ReadFile(authSSHKeyFile)
				if err != nil {
					return fmt.Errorf("read --auth-ssh-key %q: %w", authSSHKeyFile, err)
				}
				authType, authCredential = "ssh-key", string(pem)
			}

			st, err := newWatcherClient().CreateStack(cmd.Context(), watcherclient.CreateStackRequest{
				Name:                name,
				RepoURL:             repo,
				Branch:              branch,
				AuthType:            authType,
				AuthCredential:      authCredential,
				ManifestPath:        manifestPath,
				PollIntervalSeconds: poll,
				AllowDuplicate:      allowDuplicate,
			})
			if err != nil {
				return err
			}
			fmt.Printf("added stack %s (repo=%s branch=%s manifest=%s)\n", st.Name, st.RepoURL, st.Branch, st.ManifestPath)
			fmt.Printf("its projects deploy into namespace %s on network qooq-%s\n", st.Name, st.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "stack name; becomes the namespace, the network and the container name prefix (required, lowercase DNS label)")
	cmd.Flags().StringVar(&repo, "repo", "", "git repository holding qooq.stack.yaml (required)")
	cmd.Flags().StringVar(&branch, "branch", "", "branch to watch (default: main)")
	cmd.Flags().StringVar(&authToken, "auth-token", "", "HTTPS auth token for the stack repo, or @file to read it from a file (mutually exclusive with --auth-ssh-key). Never forwarded to a project's external repo")
	cmd.Flags().StringVar(&authSSHKeyFile, "auth-ssh-key", "", "path to a PEM-encoded SSH private key for the stack repo (mutually exclusive with --auth-token)")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "repo-relative path to the stack manifest (default: qooq.stack.yaml)")
	cmd.Flags().IntVar(&poll, "poll", 0, "poll interval in seconds (default: 60, minimum: 10)")
	cmd.Flags().BoolVar(&allowDuplicate, "allow-duplicate", false, "deploy these projects even though another target already deploys them from the same repository (staging beside production); without it that is refused, because it is also what a stack migration looks like when the old target was left behind")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("repo")
	return cmd
}

func stackLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List stacks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ds, err := newWatcherClient().ListStacks(cmd.Context())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATUS\tREPO\tBRANCH\tMANIFEST_COMMIT\tLAST_SYNC")
			for _, st := range ds {
				lastSync := "-"
				if st.LastSyncAt != nil {
					lastSync = formatUTC(*st.LastSyncAt)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					st.Name, st.Status, st.RepoURL, st.Branch, short(st.LastManifestCommit), lastSync)
			}
			return w.Flush()
		},
	}
}

func stackGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Show a stack and every project in it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := newWatcherClient().GetStack(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			lastSync := "-"
			if st.LastSyncAt != nil {
				lastSync = formatUTC(*st.LastSyncAt)
			}
			nextCheck := "-"
			if st.NextCheckAt != nil {
				nextCheck = formatUTC(*st.NextCheckAt)
			}
			fmt.Printf("name:          %s\nstatus:        %s\nrepo_url:      %s\nbranch:        %s\nmanifest_path: %s\nmanifest_sha:  %s\nnamespace:     %s\nnetwork:       qooq-%s\npoll_interval: %ds\nlast_sync_at:  %s\nnext_check_at: %s\n",
				st.Name, st.Status, st.RepoURL, st.Branch, st.ManifestPath, dashIfEmpty(short(st.LastManifestCommit)),
				st.Name, st.Name, st.PollIntervalSeconds, lastSync, nextCheck)
			if st.PausedReason != "" {
				fmt.Printf("paused:        %s\n", st.PausedReason)
			}
			if st.LastError != "" {
				fmt.Printf("last_error:    %s\n", st.LastError)
			}
			fmt.Println(stackStatusHint(st.Status))

			if len(st.Projects) == 0 {
				return nil
			}
			fmt.Println()
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "PROJECT\tDEPENDS_ON\tSTATUS\tCOMMIT\tFAILURES\tSOURCE")
			for _, s := range st.Projects {
				name := s.Name
				if s.Orphaned {
					name += " (orphaned)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					name, dashIfEmpty(strings.Join(s.DependsOn, ", ")), s.LastBuildStatus,
					dashIfEmpty(short(s.LastCommit)), failureCount(s.ConsecutiveFailures), s.Source)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			for _, s := range st.Projects {
				if s.LastBuildError != "" {
					fmt.Printf("\n%s: %s\n", s.Name, s.LastBuildError)
				}
			}
			return nil
		},
	}
}

// stackStatusHint explains a status in the terms that matter: what is running
// right now, and what happens next.
func stackStatusHint(status string) string {
	switch status {
	case "degraded":
		return "\nsome projects deployed and others did not, so this stack is running a mix of versions.\nThe projects that failed retry on the next poll; the ones that were skipped follow once their upstream is back."
	case "paused":
		return "\ndeployment is stopped for every project — everything already running keeps running.\nFix the project named above, then `qooq stack resume <name>`."
	case "failed":
		return "\nnothing deployed this pass. Whatever was already running is untouched."
	default:
		return ""
	}
}

func failureCount(n int) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", n)
}

func stackRmCmd() *cobra.Command {
	var prune bool
	cmd := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Remove a stack (its components keep running unless --prune)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := newWatcherClient().DeleteStack(cmd.Context(), args[0], prune); err != nil {
				return err
			}
			if prune {
				fmt.Printf("removed stack %s and its components\n", args[0])
				fmt.Println("its services and volumes were left alone; remove those by hand if you mean to")
				return nil
			}
			fmt.Printf("removed stack %s; its components are still running\n", args[0])
			fmt.Println("pass --prune to remove them too, or use `qooq component rm` one at a time")
			return nil
		},
	}
	cmd.Flags().BoolVar(&prune, "prune", false, "also remove the deployed components (services and volumes are still left alone)")
	return cmd
}

func stackTriggerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "trigger <name>",
		Short: "Deploy this stack now, bypassing its poll interval",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := newWatcherClient().TriggerStack(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("triggered %s\n", args[0])
			return nil
		},
	}
}

func stackResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <name>",
		Short: "Lift a pause and give every project a fresh failure budget",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := newWatcherClient().ResumeStack(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Printf("resumed %s; it will be deployed on the next sweep\n", st.Name)
			return nil
		},
	}
}

func stackManifestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "manifest <name>",
		Short: "Print the stack manifest the watcher is working from",
		Long: "Print the stack manifest the watcher is working from.\n\n" +
			"This is the last manifest that parsed, which is not necessarily what is\n" +
			"on the branch right now: a commit that breaks it leaves the previous one\n" +
			"in force rather than tearing the stack down.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := newWatcherClient().StackManifest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(data)
			return err
		},
	}
}
