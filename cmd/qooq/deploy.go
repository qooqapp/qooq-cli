package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qooqapp/qooq/pkg/controlclient"
	"github.com/qooqapp/qooq/pkg/manifest"
)

// serviceName is what a name must look like: it becomes a hostname label.
var serviceName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

var nonLabel = regexp.MustCompile(`[^a-z0-9]+`)

// deployName picks the service name: --name wins, then qooq.yaml's `app`,
// then the directory's own name. A name that cannot be made valid is an error
// that says what to do, rather than a guess.
func deployName(dir, flagName string) (string, error) {
	if flagName != "" {
		if !serviceName.MatchString(flagName) {
			return "", fmt.Errorf("--name %q is not a valid name: use lowercase letters, digits and hyphens, not starting or ending with a hyphen", flagName)
		}
		return flagName, nil
	}
	if data, err := os.ReadFile(filepath.Join(dir, "qooq.yaml")); err == nil {
		m, perr := manifest.Parse(data)
		if perr != nil {
			return "", fmt.Errorf("qooq.yaml: %w", perr)
		}
		if m.App != "" {
			if !serviceName.MatchString(m.App) {
				return "", fmt.Errorf("qooq.yaml names the app %q, which is not a valid service name (lowercase letters, digits, hyphens): fix it there or pass --name", m.App)
			}
			return m.App, nil
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	n := strings.Trim(nonLabel.ReplaceAllString(strings.ToLower(filepath.Base(abs)), "-"), "-")
	if len(n) > 63 {
		n = strings.Trim(n[:63], "-")
	}
	if !serviceName.MatchString(n) {
		return "", fmt.Errorf("cannot make a service name out of the directory name %q: pass --name", filepath.Base(abs))
	}
	return n, nil
}

func deployDirCmd() *cobra.Command {
	var o projectOpts
	var name, ns string
	var force, noWait, cloud bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "deploy [dir]",
		Short: "Deploy a directory — no git needed",
		Long: `Package DIR (default: the current directory), upload it, build and deploy it,
and print the URL it is running at. The service is created on the first deploy;
every later one updates it.

Where it goes: to qooq on this machine (start it with ` + "`qooq up`" + `), or with
--cloud to the qooq Cloud server you signed in to with ` + "`qooq auth login`" + `.

What is uploaded:
  - in a git repository: tracked and untracked files that .gitignore lets through
  - otherwise: everything except .git, node_modules, .venv, __pycache__, .DS_Store
  - then .qooqignore (gitignore syntax) removes more
  - .env* files are never uploaded: put secrets in the namespace's secret store
    and reference them from qooq.yaml as ${secret.NAME}

The service name is qooq.yaml's "app", else the directory name (or --name).
Deploying content that is identical to what is already running does nothing;
--force rebuilds anyway.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			if !cloud {
				return runLocalDeploy(cmd, dir, name, ns, force, noWait, timeout, o.output)
			}
			return runDeploy(cmd, &o, dir, name, ns, force, noWait, timeout)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&cloud, "cloud", false, "deploy to qooq Cloud instead of this machine")
	f.StringVar(&watcherAddr, "watcher-addr", watcherAddr, "qooq-watcher base URL for a local deploy (default: QOOQ_WATCHER_ADDR env)")
	f.StringVar(&o.addr, "addr", "", "with --cloud: qooq Cloud address (default: $QOOQ_CONTROL_ADDR, else "+defaultControlAddr+" for a local development server)")
	f.StringVar(&o.account, "account", "", "with --cloud: account slug (default: the account you signed in with)")
	f.StringVarP(&o.output, "output", "o", "table", "output format: table|json")
	f.StringVar(&name, "name", "", "service name (default: qooq.yaml's app, else the directory name)")
	f.StringVarP(&ns, "namespace", "n", "default", "namespace")
	f.BoolVar(&force, "force", false, "build even when the content is identical to what is deployed")
	f.BoolVar(&noWait, "no-wait", false, "return after the upload instead of waiting for the build")
	f.DurationVar(&timeout, "timeout", 15*time.Minute, "how long to wait for the build")
	return cmd
}

func runDeploy(cmd *cobra.Command, o *projectOpts, dir, flagName, ns string, force, noWait bool, timeout time.Duration) error {
	errw := cmd.ErrOrStderr()
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	name, err := deployName(dir, flagName)
	if err != nil {
		return err
	}
	c, acct, err := o.connect()
	if err != nil {
		return err
	}

	set, err := listFiles(dir)
	if err != nil {
		return err
	}
	if len(set.EnvSkipped) > 0 {
		fmt.Fprintf(errw, "not uploading %d env file(s): %s — secrets do not belong in an upload; set them in the namespace's secret store and reference ${secret.NAME} in qooq.yaml\n",
			len(set.EnvSkipped), strings.Join(set.EnvSkipped, ", "))
	}
	if len(set.Files) == 0 {
		return errors.New("nothing to upload: the directory has no files once .gitignore, .qooqignore and the .env rule are applied")
	}
	if err := checkBeforePacking(dir, set.Files); err != nil {
		return err
	}
	pk, err := pack(dir, set.Files)
	if err != nil {
		return err
	}
	defer os.Remove(pk.Path)
	fmt.Fprintf(errw, "packed %d files, %s\n", pk.Count, humanSize(pk.Compressed))
	if pk.Compressed > uploadLimit {
		return errors.New(pk.tooLargeMessage())
	}

	// Baseline for --wait. The upload response carries the authoritative
	// generation, but LastBuildAt can only come from before: with it unset, a
	// previous deploy's finished build would read as the new one.
	var base controlclient.Project
	if ps, err := c.ListProjects(cmd.Context(), acct); err == nil {
		for _, p := range ps {
			if p.Name == name && p.Namespace == ns {
				base = p
			}
		}
	} else {
		return o.explain(err)
	}
	if base.Name == "" {
		base = controlclient.Project{Name: name, Namespace: ns}
	} else if base.Source != "upload" {
		return fmt.Errorf("%s/%s already exists as a %s service, so a directory cannot be uploaded to it: pick another name with --name, or remove it first", ns, name, base.Source)
	}

	f, err := os.Open(pk.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(errw, "uploading to %s/%s ...\n", ns, name)
	res, err := c.UploadProjectSource(cmd.Context(), acct, ns, name, f, pk.Compressed, force)
	if err != nil {
		return o.explain(err)
	}
	if res.Created {
		fmt.Fprintf(errw, "created %s/%s\n", ns, name)
	}

	if !res.BuildQueued {
		// Same content as what is deployed: nothing will build, and waiting for
		// a new build would wait forever.
		cur := base
		if ps, err := c.ListProjects(cmd.Context(), acct); err == nil {
			for _, p := range ps {
				if p.Name == name && p.Namespace == ns {
					cur = p
				}
			}
		}
		fmt.Fprintln(errw, "the content has not changed, so nothing was redeployed (use --force to rebuild anyway)")
		if len(cur.Endpoints) > 0 {
			fmt.Fprintln(errw, "running at: "+strings.Join(cur.Endpoints, ", "))
		}
		return o.print(cmd.OutOrStdout(), cur)
	}
	fmt.Fprintf(errw, "uploaded revision %s\n", shortCommit(res.Revision))
	if noWait {
		if o.output == "json" {
			return writeJSON(cmd.OutOrStdout(), res)
		}
		return nil
	}
	base.BuildGeneration = res.BuildGeneration
	return o.waitAndReport(cmd, c, acct, base, timeout)
}
