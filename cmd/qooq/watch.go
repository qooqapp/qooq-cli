package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/qooqapp/qooq/pkg/client"
	"github.com/qooqapp/qooq/pkg/watcherclient"
)

// defaultWatcherAddr is qooq-watcher's base URL; overridable with
// --watcher-addr or QOOQ_WATCHER_ADDR.
const defaultWatcherAddr = "http://127.0.0.1:7778"

var watcherAddr, watcherToken string

func newWatcherClient() *watcherclient.Client {
	return watcherclient.New(watcherAddr, watcherclient.WithToken(watcherToken))
}

// watchCmd is the `qooq watch` subtree: manage qooq-watcher GitOps targets.
// It talks to qooq-watcher only through pkg/watcherclient, mirroring how the
// rest of this CLI talks to qooqd only through pkg/client.
func watchCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "watch", Short: "Manage qooq-watcher GitOps watch targets"}

	envAddr := os.Getenv("QOOQ_WATCHER_ADDR")
	if envAddr == "" {
		envAddr = defaultWatcherAddr
	}
	cmd.PersistentFlags().StringVar(&watcherAddr, "watcher-addr", envAddr, "qooq-watcher base URL (default: QOOQ_WATCHER_ADDR env)")
	cmd.PersistentFlags().StringVar(&watcherToken, "watcher-token", "", "bearer token for qooq-watcher API (default: QOOQ_WATCHER_TOKEN env)")
	watcherToken = os.Getenv("QOOQ_WATCHER_TOKEN") // not the flag default: --help prints that (see main.go)

	cmd.AddCommand(watchLogsCmd(), watchBuildLogCmd())
	cmd.AddCommand(watchAddCmd(), watchLsCmd(), watchGetCmd(), watchRmCmd(), watchServiceRmCmd(), watchTriggerCmd(), watchBranchCmd(), watchSetInputCmd(), watchDeployCmd(), watchBackupCmd(), watchManifestCmd(), watchRedeployCmd(),
		watchStopCmd(), watchStartCmd(), watchPauseCmd(), watchResumeCmd())
	return cmd
}

func watchAddCmd() *cobra.Command {
	var (
		name, repo, branch                   string
		authToken, authSSHKeyFile            string
		dockerfile, contextSubdir, component string
		manifestPath                         string
		poll                                 int
		webhookSecret, deploySpec, namespace string
		deployMode, imageRef                 string
		inputs                               []string
		allowDuplicate                       bool
	)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Register a new GitOps watch target",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// @file support (same convention as --deploy-spec): read the
			// secret from a file rather than passing it on the command line.
			var err error
			if authToken, err = atFileValue(authToken); err != nil {
				return fmt.Errorf("--auth-token: %w", err)
			}
			if webhookSecret, err = atFileValue(webhookSecret); err != nil {
				return fmt.Errorf("--webhook-secret: %w", err)
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

			var rawSpec json.RawMessage
			if deploySpec != "" {
				data, err := loadDeploySpec(deploySpec)
				if err != nil {
					return err
				}
				if !json.Valid(data) {
					return fmt.Errorf("--deploy-spec is not valid JSON")
				}
				rawSpec = json.RawMessage(data)
			}
			if namespace != "" {
				rawSpec, err = withNamespace(rawSpec, namespace)
				if err != nil {
					return err
				}
			}

			inputValues, err := parseInputFlags(inputs)
			if err != nil {
				return err
			}

			req := watcherclient.CreateTargetRequest{
				Name:                name,
				DeployMode:          deployMode,
				RepoURL:             repo,
				Branch:              branch,
				AuthType:            authType,
				AuthCredential:      authCredential,
				Dockerfile:          dockerfile,
				ContextSubdir:       contextSubdir,
				ManifestPath:        manifestPath,
				ComponentName:       component,
				AllowDuplicate:      allowDuplicate,
				ImageRef:            imageRef,
				DeploySpec:          rawSpec,
				PollIntervalSeconds: poll,
				WebhookSecret:       webhookSecret,
				Inputs:              inputValues,
			}
			t, err := newWatcherClient().CreateTarget(cmd.Context(), req)
			if err != nil {
				return err
			}
			if t.DeployMode == "image" {
				fmt.Printf("added target %s (deploy_mode=image image_ref=%s component=%s)\n", t.Name, t.ImageRef, t.ComponentName)
			} else {
				fmt.Printf("added target %s (repo=%s branch=%s component=%s)\n", t.Name, t.RepoURL, t.Branch, t.ComponentName)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "target name (required)")
	cmd.Flags().StringVar(&deployMode, "deploy-mode", "", `"source" (default) clones/builds repo; "image" polls a registry ref's digest and deploys it directly (requires --image-ref, rejects --repo/--branch/--dockerfile/--context-subdir/--manifest)`)
	cmd.Flags().StringVar(&repo, "repo", "", "git repository URL (required in source mode; must be unset in image mode)")
	cmd.Flags().StringVar(&branch, "branch", "", "branch to watch (default: main; source mode only)")
	cmd.Flags().StringVar(&authToken, "auth-token", "", `HTTPS auth token, or @file to read it from a file (source mode; mutually exclusive with --auth-ssh-key). In image mode, this is instead a registry credential as "username:password" (no colon = username only)`)
	cmd.Flags().StringVar(&authSSHKeyFile, "auth-ssh-key", "", "path to a PEM-encoded SSH private key file (source mode only, mutually exclusive with --auth-token)")
	cmd.Flags().StringVar(&dockerfile, "dockerfile", "", "Dockerfile path relative to the build context (default: Dockerfile; source mode only)")
	cmd.Flags().StringVar(&contextSubdir, "context-subdir", "", "build context subdirectory within the repo (source mode only)")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", `repo-relative path to the app manifest (default: qooq.yaml; "-" disables manifest parsing for this target; source mode only)`)
	cmd.Flags().StringVar(&component, "component", "", "qooqd component name to build and deploy (required)")
	cmd.Flags().StringVar(&imageRef, "image-ref", "", `registry ref to poll for digest changes, e.g. "ghcr.io/acme/api:latest" (required in image mode, must be unset in source mode)`)
	cmd.Flags().IntVar(&poll, "poll", 0, "poll interval in seconds (default: 60, minimum: 10)")
	cmd.Flags().StringVar(&webhookSecret, "webhook-secret", "", "shared secret for webhook signature verification, or @file to read it from a file (unset disables webhooks for this target)")
	cmd.Flags().BoolVar(&allowDuplicate, "allow-duplicate", false, "deploy this component even though another target already deploys it from the same repository (staging beside production); without it that is refused, because it is also what a stack migration looks like when the old target was left behind")
	cmd.Flags().StringVar(&namespace, "namespace", "", "namespace scoping the component name; components in different namespaces may share a name (a stack sets this to its own name)")
	cmd.Flags().StringVar(&deploySpec, "deploy-spec", "", `deploy spec as a JSON string or @file (fields matching qooqd's DeployRequest minus name/image: command/env/ports/group_id/restart_policy/cpu_limit/mem_limit_bytes/health_check)`)
	cmd.Flags().StringArrayVar(&inputs, "input", nil, "value for an input the manifest declares, as NAME=VALUE or NAME=@file (repeatable; run `qooq watch get <name>` to see what a target needs)")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("component")
	return cmd
}

// loadDeploySpec reads a --deploy-spec value that is either a literal JSON
// string or, prefixed with "@", a path to a file containing the JSON.
func loadDeploySpec(v string) ([]byte, error) {
	if strings.HasPrefix(v, "@") {
		return os.ReadFile(v[1:])
	}
	return []byte(v), nil
}

// parseInputFlags turns repeated NAME=VALUE pairs into the map the API takes.
// A value may be "@path", read from a file — the same convention as
// --auth-token, and for the same reason: an API key on a command line ends up
// in shell history and in `ps` output.
func parseInputFlags(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		name, raw, found := strings.Cut(pair, "=")
		if !found || name == "" {
			return nil, fmt.Errorf("--input %q: expected NAME=VALUE", pair)
		}
		v, err := atFileValue(raw)
		if err != nil {
			return nil, fmt.Errorf("--input %s: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

// atFileValue resolves a secret flag value: "@path" reads the file and trims
// surrounding whitespace (same convention as --deploy-spec's @file), any
// other value is returned verbatim. Empty stays empty.
func atFileValue(v string) (string, error) {
	if strings.HasPrefix(v, "@") {
		data, err := os.ReadFile(v[1:])
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}
	return v, nil
}

func watchLsCmd() *cobra.Command {
	var sortBy string
	var reverse bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List GitOps watch targets",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ts, err := newWatcherClient().ListTargets(cmd.Context())
			if err != nil {
				return err
			}
			less, err := targetSorter(sortBy)
			if err != nil {
				return err
			}
			sort.SliceStable(ts, func(i, j int) bool {
				if reverse {
					return less(&ts[j], &ts[i])
				}
				return less(&ts[i], &ts[j])
			})
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			// TARGET leads: it is this table's own subject and what
			// watch get/rm/trigger take. NAMESPACE and COMPONENT follow as the
			// same two columns `component ls` prints, in the same order and
			// spelled the same way — together they are the one thing that joins
			// the two tables, so a row here can be matched to a row there by
			// eye without translating anything.
			fmt.Fprintln(w, "TARGET\tNAMESPACE\tCOMPONENT\tMODE\tREPO\tBRANCH\tLAST_COMMIT\tSTATUS\tLAST_BUILD")
			for _, t := range ts {
				mode := t.DeployMode
				if mode == "" {
					mode = "source"
				}
				repoOrImage := t.RepoURL
				lastCommitOrDigest := short(t.LastCommit)
				if mode == "image" {
					repoOrImage = t.ImageRef
					lastCommitOrDigest = short(t.LastImageDigest)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.Name, client.NormalizeNamespace(t.Namespace), t.ComponentName, mode, repoOrImage, t.Branch, lastCommitOrDigest, buildStatus(t), formatAge(t.LastBuildAt))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&sortBy, "sort", "target", "sort by: target, namespace, component, status, last-build")
	cmd.Flags().BoolVarP(&reverse, "reverse", "r", false, "reverse the sort order")
	return cmd
}

// targetSorter returns the less function for a --sort field. Rows a field
// cannot order (a target that has never built has no build time) sort last in
// either direction, so "sort by last-build" never buries the rows that do have
// one behind the ones that do not.
func targetSorter(field string) (func(a, b *watcherclient.TargetResponse) bool, error) {
	byName := func(a, b *watcherclient.TargetResponse) bool { return a.Name < b.Name }
	switch field {
	case "target", "name":
		return byName, nil
	case "component", "namespace":
		// Namespace first so each namespace's targets stay together, matching
		// how `component ls --sort name` groups the other side of the join.
		return func(a, b *watcherclient.TargetResponse) bool {
			na, nb := client.NormalizeNamespace(a.Namespace), client.NormalizeNamespace(b.Namespace)
			if na != nb {
				return na < nb
			}
			if a.ComponentName != b.ComponentName {
				return a.ComponentName < b.ComponentName
			}
			return byName(a, b)
		}, nil
	case "status":
		return func(a, b *watcherclient.TargetResponse) bool {
			if a.LastBuildStatus != b.LastBuildStatus {
				return a.LastBuildStatus < b.LastBuildStatus
			}
			return byName(a, b)
		}, nil
	case "last-build":
		return func(a, b *watcherclient.TargetResponse) bool {
			ta, tb := a.LastBuildAt, b.LastBuildAt
			if ta == nil || tb == nil {
				if ta != tb {
					return ta != nil // a target that has never built sorts last
				}
				return byName(a, b)
			}
			if !ta.Equal(*tb) {
				return ta.Before(*tb) // oldest build first
			}
			return byName(a, b)
		}, nil
	default:
		return nil, fmt.Errorf("unknown --sort field %q: use target, namespace, component, status or last-build", field)
	}
}

// buildStatus is the STATUS column: the last build's result, plus the fact
// that nothing is being deployed any more when that is true.
//
// It has to be visible HERE, in the listing, for the same reason
// `component ls` prints "Running (stale addr)": the difficulty with both is
// that every other signal says normal. A target whose deploys are switched off
// reports its last build as a success for as long as somebody leaves it that
// way, and a listing that showed only that would be telling the truth while
// answering the wrong question.
//
// Two different suffixes because they are two different situations: one is a
// decision somebody made and can undo, the other is the watcher giving up
// after repeated failures. Both are in `watch get` in full.
func buildStatus(t watcherclient.TargetResponse) string {
	switch {
	case t.PausedReason != "":
		return t.LastBuildStatus + " (gave up)"
	case t.DeployPaused:
		return t.LastBuildStatus + " (paused)"
	}
	return t.LastBuildStatus
}

func watchGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Show a GitOps watch target",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := newWatcherClient().GetTarget(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			lastBuildAt := "-"
			if t.LastBuildAt != nil {
				lastBuildAt = formatUTC(*t.LastBuildAt)
			}
			nextCheckAt := "-"
			if t.NextCheckAt != nil {
				nextCheckAt = formatUTC(*t.NextCheckAt)
			}
			mode := t.DeployMode
			if mode == "" {
				mode = "source"
			}
			fmt.Printf("name:              %s\ndeploy_mode:       %s\nrepo_url:          %s\nbranch:            %s\nauth_type:         %s\ndockerfile:        %s\ncontext_subdir:    %s\nmanifest_path:     %s\ncomponent:         %s\n",
				t.Name, mode, t.RepoURL, t.Branch, t.AuthType, t.Dockerfile, dashIfEmpty(t.ContextSubdir), dashIfEmpty(t.ManifestPath), targetComponentRef(t))
			if t.ImageRef != "" {
				fmt.Printf("image_ref:         %s\n", t.ImageRef)
			}
			if t.LastImageDigest != "" {
				fmt.Printf("last_image_digest: %s\n", t.LastImageDigest)
			}
			fmt.Printf("poll_interval:     %ds\nwebhook_secret:    %s\nlast_commit:       %s\nlast_build_status: %s\n",
				t.PollIntervalSeconds, dashIfEmpty(t.WebhookSecret), dashIfEmpty(t.LastCommit), t.LastBuildStatus)
			// Always printed, even when it equals last_commit. It is only
			// interesting when they differ — but a line that appears only
			// sometimes is a trap for anything parsing this: the field goes
			// missing exactly when a build succeeds, and the reader silently
			// gets an empty string instead of the revision it asked about.
			fmt.Printf("last_build_revision: %s\nbuild_generation:  %d\n",
				dashIfEmpty(t.LastBuildRevision), t.BuildGeneration)
			fmt.Printf("last_build_error:  %s\nlast_build_at:     %s\nnext_check_at:     %s\ncreated:           %s\n",
				dashIfEmpty(t.LastBuildError), lastBuildAt, nextCheckAt, formatUTC(t.CreatedAt))
			// Always printed, both of them, for the reason the revision above
			// is: a field that appears only sometimes goes missing exactly
			// when it matters, and these two are the answer to "why has
			// nothing been deployed since Tuesday".
			//
			// Kept apart because they are different situations with different
			// cures — `watch resume` for the first, fixing the repository and
			// `watch trigger` for the second.
			fmt.Printf("deploy_paused:     %t\npaused_reason:     %s\n",
				t.DeployPaused, dashIfEmpty(t.PausedReason))
			// How far from that pause it is: the count it is decided on.
			fmt.Printf("failures_in_row:   %d (paused at 10)\n", t.ConsecutiveFailures)
			printInputs(t)
			printDroppedServices(t)
			return nil
		},
	}
}

// printDroppedServices warns about services the manifest no longer declares:
// stopped, with their data still on disk until someone deletes them.
func printDroppedServices(t *watcherclient.TargetResponse) {
	if t.Topology == nil {
		return
	}
	for _, s := range t.Topology.Services {
		if s.Dropped {
			fmt.Printf("dropped_service:   %s (%s) is no longer in qooq.yaml: stopped, data kept.\n"+
				"                   Delete it and its data: qooq watch service-rm %s %s\n", s.Name, s.Component, t.Name, s.Name)
		}
	}
}

// watchServiceRmCmd deletes a service the manifest no longer declares, with
// its data. Deploys only ever stop such a service; deleting its data needs
// the name typed back, the terminal's version of the console's dialog.
func watchServiceRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "service-rm <target> <service>",
		Short: "Delete a service no longer in qooq.yaml, with its data (asks you to type its name)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, service := args[0], args[1]
			fmt.Printf("This permanently deletes service %q of %s: its container and all of its data.\n"+
				"It cannot be undone. Type the service name to confirm: ", service, target)
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.TrimSpace(line) != service {
				return fmt.Errorf("not confirmed; nothing was deleted")
			}
			if err := newWatcherClient().DeleteService(cmd.Context(), target, service); err != nil {
				return err
			}
			fmt.Printf("deleted service %s of %s and its data\n", service, target)
			return nil
		},
	}
}

// printInputs renders a target's declared inputs, missing ones first.
//
// What is missing leads, because when a target is stuck on a missing value
// that is the only thing the reader needs; everything above it in `watch get`
// is context they already have. Nothing prints at all for the common case of
// a manifest that declares no inputs.
func printInputs(t *watcherclient.TargetResponse) {
	if len(t.Inputs) == 0 {
		return
	}
	if len(t.MissingInputs) > 0 {
		missing := make(map[string]bool, len(t.MissingInputs))
		for _, name := range t.MissingInputs {
			missing[name] = true
		}
		fmt.Printf("\nmissing inputs (deploy is blocked until these are supplied):\n")
		for _, in := range t.Inputs {
			if !missing[in.Name] {
				continue
			}
			fmt.Printf("  %s", in.Name)
			if in.Description != "" {
				fmt.Printf(" — %s", in.Description)
			}
			if in.Example != "" {
				fmt.Printf(" (e.g. %s)", in.Example)
			}
			fmt.Println()
		}
		fmt.Printf("  supply with: qooq watch set-input %s %s=<value>\n", t.Name, t.MissingInputs[0])
	}

	fmt.Println("\ninputs:")
	for _, in := range t.Inputs {
		value := "(unset)"
		switch {
		case in.Value != "":
			value = in.Value
		case in.Set:
			value = "(set)"
		case in.Default != "":
			value = "(default: " + in.Default + ")"
		}
		flags := ""
		switch {
		case in.Required && in.Secret:
			flags = " [required, secret]"
		case in.Required:
			flags = " [required]"
		case in.Secret:
			flags = " [secret]"
		}
		fmt.Printf("  %-20s %s%s\n", in.Name, value, flags)
	}
}

func watchSetInputCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-input <name> NAME=VALUE [NAME=VALUE ...]",
		Short: "Supply values for the inputs a target's manifest declares",
		Long: "Supply values for the inputs a target's manifest declares.\n\n" +
			"Values merge into what the target already has, so setting one input does not\n" +
			"clear the others; an empty value (NAME=) removes one. Use NAME=@file to read a\n" +
			"value from a file instead of the command line, which keeps secrets out of shell\n" +
			"history. Setting a value redeploys immediately rather than waiting for the poll\n" +
			"interval — the point of supplying it is to unblock a deploy that is stuck now.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			values, err := parseInputFlags(args[1:])
			if err != nil {
				return err
			}
			t, err := newWatcherClient().SetInputs(cmd.Context(), args[0], values)
			if err != nil {
				return err
			}
			var set, cleared []string
			for name, v := range values {
				if v == "" {
					cleared = append(cleared, name)
					continue
				}
				set = append(set, name)
			}
			sort.Strings(set)
			sort.Strings(cleared)
			if len(set) > 0 {
				fmt.Printf("set %s on %s\n", strings.Join(set, ", "), t.Name)
			}
			if len(cleared) > 0 {
				fmt.Printf("cleared %s on %s\n", strings.Join(cleared, ", "), t.Name)
			}
			if len(t.MissingInputs) > 0 {
				// The deploy was triggered regardless, but it will stop at the
				// same check, so say so rather than leaving "redeploying" as
				// the last word.
				fmt.Printf("still missing: %s — deploy stays blocked\n", strings.Join(t.MissingInputs, ", "))
				return nil
			}
			fmt.Println("redeploying")
			return nil
		},
	}
}

func watchRmCmd() *cobra.Command {
	var keep bool
	cmd := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Remove a watch target and the deployment it made",
		Long: `Remove a watch target and the component it deployed.

The container is stopped and removed and its address stops working. Its data
goes too: the volumes qooq created for it, including the database of every
service it declared, and the images qooq built for it.

--keep-component removes only the watching and leaves the deployment running,
which is what you want when handing it over to a stack or another watcher.
That used to be what this command did unconditionally, which left containers
running that nothing was watching or listing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := newWatcherClient().DeleteTarget(cmd.Context(), args[0], keep); err != nil {
				return err
			}
			// Says which of the two happened, because the difference is a
			// container that is or is not still serving.
			if keep {
				// "left in place", not "still running": the component may well
				// have been stopped, and this command did not change that.
				fmt.Printf("removed target %s; its deployment was left in place\n", args[0])
			} else {
				fmt.Printf("removed target %s and its deployment\n", args[0])
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&keep, "keep-component", false,
		"leave the deployed component running; remove only the watch target")
	return cmd
}

// The three things you can do to a deployment that are not "build it again".
//
// Four commands rather than two toggles: a verb that means "set this to the
// opposite of whatever it is now" cannot be used from a script without reading
// the state first, and reading-then-toggling races with anything else doing
// the same. It also makes `watch stop` safe to run twice, which is what
// somebody does when they are not sure the first one worked.
//
// stop/start and pause/resume are deliberately separate pairs. One acts on the
// process, the other on the watching; a target that is stopped but still
// watched comes back up the next time the repository moves, because a deploy
// starts what it deploys. Saying so is this file's job — the behaviour is
// right, it is just surprising.

func watchStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop the deployment, keeping the target and its images",
		Long: `Stop the component this target deploys.

The target, its settings, its images and any volumes stay exactly where they
are; what stops is the process answering at the address.

This is not the same as pausing. If the target is still being watched, the next
push rebuilds and starts it again — a deploy starts what it deploys. To stop it
staying stopped, run ` + "`qooq watch pause <name>`" + ` as well.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := newWatcherClient().StopTarget(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("stopped %s\n", args[0])
			return nil
		},
	}
}

func watchStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start <name>",
		Short: "Start a stopped deployment again",
		Long: `Start the component this target deploys, from the image of its last
successful build. Nothing is rebuilt; use ` + "`qooq watch trigger`" + ` for that.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := newWatcherClient().StartTarget(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("started %s\n", args[0])
			return nil
		},
	}
}

func watchPauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause <name>",
		Short: "Stop deploying this target automatically",
		Long: `Take this target out of the sweep, so changes on the remote are no longer
deployed automatically.

What is running keeps running and keeps serving. An explicit
` + "`qooq watch trigger --force <name>`" + ` still builds it: pausing automatic
deploys is not refusing to deploy, and a pause that swallowed a deliberate
deploy would do nothing on exactly the target somebody is fixing.

Separate from the pause the watcher applies itself after repeated build
failures (` + "`paused_reason`" + ` in ` + "`watch get`" + `). One is a decision, the
other is a breakdown.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := newWatcherClient().PauseTarget(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("paused automatic deploys for %s\n", args[0])
			return nil
		},
	}
}

func watchResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <name>",
		Short: "Resume automatic deploys",
		Long: `Put this target back in the sweep and check it immediately — the reason to
resume is usually that you want what was pushed while it was paused, and
waiting out the poll interval looks like the resume did not work.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := newWatcherClient().ResumeTarget(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("resumed automatic deploys for %s; checking now\n", args[0])
			return nil
		},
	}
}

func watchTriggerCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "trigger <name>",
		Short: "Check a watch target now, bypassing its poll interval",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := newWatcherClient().TriggerTarget(cmd.Context(), args[0], force); err != nil {
				return err
			}
			fmt.Printf("triggered %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "rebuild even if the remote has not changed")
	return cmd
}

func watchBranchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "branch <name> <branch>",
		Short: "Switch a watch target to another branch and rebuild from its HEAD",
		Long: "Points an existing target at another branch and rebuilds it right away. " +
			"The component is kept (same name, same address). Branch names only, not tags. " +
			"A target that belongs to a stack is refused: change the branch in qooq.stack.yaml.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := newWatcherClient().SetBranch(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			fmt.Printf("%s now follows %s; rebuilding on the next sweep tick\n", t.Name, t.Branch)
			return nil
		},
	}
}

func watchDeployCmd() *cobra.Command {
	var image string
	cmd := &cobra.Command{
		Use:   "deploy <name>",
		Short: "Deploy a specific image to a target right now (bypasses the poll/build pipeline)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := newWatcherClient().DeployImage(cmd.Context(), args[0], image)
			if err != nil {
				return err
			}
			// A version that did not start is rolled back and answered as
			// failed: say why and exit non-zero, so a CI step fails with it.
			if t.LastBuildStatus == "failed" {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s\n", t.LastBuildError)
				return fmt.Errorf("deploying %s to target %s failed", image, t.Name)
			}
			fmt.Printf("deployed %s to target %s (component=%s status=%s)\n", image, t.Name, t.ComponentName, t.LastBuildStatus)
			return nil
		},
	}
	cmd.Flags().StringVar(&image, "image", "", "image ref to pull and deploy (required)")
	_ = cmd.MarkFlagRequired("image")
	return cmd
}

func watchBackupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backup <output-file>",
		Short: "Write a consistent snapshot of the watcher database to a file (contains credentials)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 0600: the snapshot contains credentials; don't leave it world-readable.
			f, err := os.OpenFile(args[0], os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			defer f.Close()
			if err := newWatcherClient().Backup(cmd.Context(), f); err != nil {
				os.Remove(args[0]) // don't leave a misleading partial file
				return err
			}
			fmt.Printf("watcher backup written to %s\n", args[0])
			return nil
		},
	}
}

// watchManifestCmd shows, and replaces, the manifest qooq generated for a repo
// that has no qooq.yaml of its own.
//
// The correction loop this exists for: qooq guesses, the guess is wrong, you
// fix it here, and the fix sticks — generation only ever runs when nothing is
// stored.
func watchManifestCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "manifest <name>",
		Short: "Show or replace the manifest qooq generated for a target",
		Long: "Repos with no qooq.yaml get one generated on their first build. This shows it,\n" +
			"and --file replaces it. Your replacement is never regenerated over.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newWatcherClient()
			if file == "" {
				data, err := c.GetManifest(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				fmt.Print(string(data))
				return nil
			}

			body, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			if _, err := c.PutManifest(cmd.Context(), args[0], body); err != nil {
				return err
			}
			fmt.Printf("replaced the generated manifest for %s (%d bytes)\n", args[0], len(body))
			fmt.Println("rebuilding now — watch it with: qooq watch get " + args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "replace the generated manifest with the contents of this file")
	return cmd
}

// formatAge renders how long ago something happened, coarsely — "when did this
// last build" is a question about minutes and hours, not timestamps, and the
// answer has to stay narrow enough to sit in a table column.
func formatAge(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	d := time.Since(*t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// targetComponentRef is the component this target deploys to, fully qualified.
// `watch get` uses it so the value can be pasted straight into a `component`
// subcommand; the ls tables print the two halves as separate columns instead,
// so they can be scanned down.
func targetComponentRef(t *watcherclient.TargetResponse) string {
	return client.NormalizeNamespace(t.Namespace) + "/" + t.ComponentName
}

// withNamespace folds --namespace into the deploy spec, which is where the
// watcher reads it from. Setting it in both places is rejected rather than
// silently resolved: the two would disagree about which component this target
// owns, and only one of them can be right.
func withNamespace(spec json.RawMessage, namespace string) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if len(spec) > 0 {
		if err := json.Unmarshal(spec, &fields); err != nil {
			return nil, fmt.Errorf("--deploy-spec must be a JSON object to combine with --namespace: %w", err)
		}
		if existing, ok := fields["namespace"]; ok {
			var got string
			if err := json.Unmarshal(existing, &got); err != nil || got != namespace {
				return nil, fmt.Errorf("--namespace %q conflicts with the namespace in --deploy-spec; set it in one place", namespace)
			}
		}
	}
	encoded, err := json.Marshal(namespace)
	if err != nil {
		return nil, err
	}
	fields["namespace"] = encoded
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}

// watchLogsCmd prints what the deployed app wrote, by target name — the
// address `qooq deploy` and `watch ls` give, so nobody has to translate it
// into a namespace/component pair first.
func watchLogsCmd() *cobra.Command {
	var follow bool
	var tail int
	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Print the deployed app's output (stdout and stderr)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if follow {
				var stop context.CancelFunc
				ctx, stop = signal.NotifyContext(ctx, os.Interrupt)
				defer stop()
			}
			rc, err := newWatcherClient().AppLogs(ctx, args[0], tail, follow)
			if err != nil {
				return err
			}
			defer rc.Close()
			if _, err := io.Copy(cmd.OutOrStdout(), rc); err != nil && ctx.Err() == nil {
				return err
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new output until Ctrl-C")
	cmd.Flags().IntVar(&tail, "tail", 200, "how many lines from the end to start with")
	return cmd
}

// watchBuildLogCmd prints the output of the target's most recent build —
// the Docker steps, compiler errors and all — which `watch get` only
// summarizes in one line.
func watchBuildLogCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "build-log <name>",
		Short: "Print the output of the target's most recent build",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			text, err := newWatcherClient().BuildLog(cmd.Context(), args[0])
			if err != nil {
				if watcherclient.StatusCode(err) == http.StatusNotFound {
					return fmt.Errorf("%s has not been built yet, or does not exist (`qooq watch ls`)", args[0])
				}
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), text)
			return nil
		},
	}
}
