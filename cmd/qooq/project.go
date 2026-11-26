package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/qooqapp/qooq/pkg/controlclient"
)

// `qooq project` drives qooq Cloud's project API with the token
// `qooq auth login` stored. Output is built for two readers: a table for a
// person, and with -o json the raw project object(s) for a script or an agent.
// Progress chatter goes to stderr so stdout stays parseable.

// pollEvery is a var so tests do not wait three seconds per poll.
var pollEvery = 3 * time.Second

// Build status values, as the watcher writes them.
const (
	statusNever      = "never"
	statusBuilding   = "building"
	statusSuccess    = "success"
	statusFailed     = "failed"
	statusNeedsInput = "needs_input"
)

type projectOpts struct {
	addr, account, output string
}

func projectCmd() *cobra.Command {
	var o projectOpts
	cmd := &cobra.Command{Use: "project", Short: "Create, deploy and inspect services on qooq Cloud"}
	pf := cmd.PersistentFlags()
	pf.StringVar(&o.addr, "addr", "", "qooq Cloud address (default: $QOOQ_CONTROL_ADDR, else "+defaultControlAddr+" for a local development server)")
	pf.StringVar(&o.account, "account", "", "account slug (default: the account you signed in with)")
	pf.StringVarP(&o.output, "output", "o", "table", "output format: table|json")

	// Each command gets its own namespace variable: when ls and create shared
	// one, registering create's flag wrote its default ("default") into it,
	// and ls silently listed only the default namespace.
	var lsNS string
	var wait bool
	var timeout time.Duration

	ls := &cobra.Command{
		Use: "ls", Short: "List services", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, acct, err := o.connect()
			if err != nil {
				return err
			}
			ps, err := c.ListProjects(cmd.Context(), acct)
			if err != nil {
				return o.explain(err)
			}
			if lsNS != "" {
				ps = filterNS(ps, lsNS)
			}
			sortProjects(ps)
			if ps == nil {
				ps = []controlclient.Project{}
			}
			if o.output == "json" {
				return writeJSON(cmd.OutOrStdout(), ps)
			}
			return writeTable(cmd.OutOrStdout(), ps)
		},
	}
	ls.Flags().StringVarP(&lsNS, "namespace", "n", "", "only this namespace")

	var repo, branch, image, registryAuth, authToken, createNS string
	var port int
	create := &cobra.Command{
		Use: "create NAME", Short: "Connect a repository or image and deploy it", Args: cobra.ExactArgs(1),
		Long: "Git:    qooq project create web --repo https://github.com/me/web [--branch main]\n" +
			"Image:  qooq project create web --image nginx:alpine [--port 80] [--registry-auth user:pass]",
		RunE: func(cmd *cobra.Command, args []string) error {
			if (repo == "") == (image == "") {
				return errors.New("give exactly one of --repo or --image")
			}
			req := controlclient.CreateProjectRequest{Name: args[0], Namespace: createNS}
			if image != "" {
				if branch != "" || authToken != "" {
					return errors.New("--branch and --auth-token apply to --repo, not --image")
				}
				req.Source, req.ImageRef, req.RegistryAuth, req.ContainerPort = "image", image, registryAuth, port
			} else {
				if registryAuth != "" || port != 0 {
					return errors.New("--registry-auth and --port apply to --image, not --repo")
				}
				req.Source, req.RepoURL, req.Branch, req.AuthToken = "git", repo, branch, authToken
			}
			c, acct, err := o.connect()
			if err != nil {
				return err
			}
			p, err := c.CreateProject(cmd.Context(), acct, req)
			if err != nil {
				return o.explain(err)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "created %s/%s\n", p.Namespace, p.Name)
			if !wait {
				return o.print(cmd.OutOrStdout(), *p)
			}
			return o.waitAndReport(cmd, c, acct, *p, timeout)
		},
	}
	create.Flags().StringVarP(&createNS, "namespace", "n", "default", "namespace")
	create.Flags().StringVar(&repo, "repo", "", "git repository URL")
	create.Flags().StringVar(&branch, "branch", "", "branch (default: main)")
	create.Flags().StringVar(&authToken, "auth-token", "", "token for a private repository")
	create.Flags().StringVar(&image, "image", "", "image reference to run instead of building a repo")
	create.Flags().IntVar(&port, "port", 0, "container port to publish (image services)")
	create.Flags().StringVar(&registryAuth, "registry-auth", "", "user:pass for a private registry (image services)")
	create.Flags().BoolVar(&wait, "wait", false, "wait until the deploy finishes")
	create.Flags().DurationVar(&timeout, "timeout", 15*time.Minute, "how long --wait waits")

	var gns string
	nsFlag := func(c *cobra.Command) {
		c.Flags().StringVarP(&gns, "namespace", "n", "", "namespace (optional when the name is unique)")
	}

	get := &cobra.Command{
		Use: "get NAME", Short: "Show one service in full", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, acct, err := o.connect()
			if err != nil {
				return err
			}
			p, err := o.find(cmd.Context(), c, acct, gns, args[0])
			if err != nil {
				return err
			}
			if o.output == "json" {
				return writeJSON(cmd.OutOrStdout(), p)
			}
			return writeDetail(cmd.OutOrStdout(), p)
		},
	}
	nsFlag(get)

	var dwait bool
	var dtimeout time.Duration
	var dimage string
	deploy := &cobra.Command{
		Use:   "deploy NAME",
		Short: "Rebuild and redeploy, or deploy a given image (--image)",
		Long: `Rebuild and redeploy NAME. With --image, deploy that image now instead —
how a CI pipeline picks the version it just pushed:

  qooq project deploy web -n prod --image registry.example.com/web:1.4.2

An image deploy is synchronous: it returns when the new image is running, and
the image becomes what an image service watches from then on. An image that
does not start is rolled back to the previous version, and the command exits
non-zero with the reason.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, acct, err := o.connect()
			if err != nil {
				return err
			}
			// Baseline BEFORE the trigger: until the build worker picks the
			// target up, the project still describes the previous deploy.
			base, err := o.find(cmd.Context(), c, acct, gns, args[0])
			if err != nil {
				return err
			}
			if dimage != "" {
				p, err := c.DeployProjectImage(cmd.Context(), acct, base.Namespace, base.Name, dimage)
				if err != nil {
					return o.explain(err)
				}
				// A version that did not start is rolled back and comes back
				// failed with the reason: print the project and fail, so the
				// CI step that ran this fails with it.
				if p.LastBuildStatus == "failed" {
					if err := o.print(cmd.OutOrStdout(), *p); err != nil {
						return err
					}
					return fmt.Errorf("deploying %s to %s/%s failed; the previous version is still running", dimage, base.Namespace, base.Name)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "deployed %s to %s/%s\n", dimage, base.Namespace, base.Name)
				return o.print(cmd.OutOrStdout(), *p)
			}
			if err := c.DeployProject(cmd.Context(), acct, base.Namespace, base.Name); err != nil {
				return o.explain(err)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "deploy queued for %s/%s\n", base.Namespace, base.Name)
			if !dwait {
				return nil
			}
			return o.waitAndReport(cmd, c, acct, base, dtimeout)
		},
	}
	nsFlag(deploy)
	deploy.Flags().BoolVar(&dwait, "wait", false, "wait until the deploy finishes")
	deploy.Flags().StringVar(&dimage, "image", "", "deploy this image now (synchronous; --wait is not needed)")
	deploy.Flags().DurationVar(&dtimeout, "timeout", 15*time.Minute, "how long --wait waits")

	// logs is what the app prints, like docker logs; --build is the last
	// build's output, which is what this command used to show — and the only
	// output an account could see at all.
	var buildLog, follow bool
	var tail int
	logs := &cobra.Command{
		Use: "logs NAME", Short: "Print what the running app writes (--build: the last build's output)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, acct, err := o.connect()
			if err != nil {
				return err
			}
			p, err := o.find(cmd.Context(), c, acct, gns, args[0])
			if err != nil {
				return err
			}
			if buildLog {
				text, err := c.BuildLog(cmd.Context(), acct, p.Namespace, p.Name)
				if err != nil {
					return o.explain(err)
				}
				_, err = io.WriteString(cmd.OutOrStdout(), text)
				return err
			}
			ctx := cmd.Context()
			if follow {
				var stop context.CancelFunc
				ctx, stop = signal.NotifyContext(ctx, os.Interrupt)
				defer stop()
			}
			rc, err := c.AppLogs(ctx, acct, p.Namespace, p.Name, tail, follow)
			if err != nil {
				return o.explain(err)
			}
			defer rc.Close()
			if _, err := io.Copy(cmd.OutOrStdout(), rc); err != nil && ctx.Err() == nil {
				return err
			}
			return nil
		},
	}
	nsFlag(logs)
	logs.Flags().BoolVar(&buildLog, "build", false, "print the last build's output instead")
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new lines until Ctrl-C")
	logs.Flags().IntVar(&tail, "tail", 200, "how many of the most recent lines (at most 5000)")

	var yes bool
	rm := &cobra.Command{
		Use: "rm NAME", Short: "Delete a service and stop what it runs", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return errors.New("this deletes the service and removes the running deployment; pass --yes to confirm")
			}
			c, acct, err := o.connect()
			if err != nil {
				return err
			}
			p, err := o.find(cmd.Context(), c, acct, gns, args[0])
			if err != nil {
				return err
			}
			if err := c.DeleteProject(cmd.Context(), acct, p.Namespace, p.Name); err != nil {
				return o.explain(err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s/%s\n", p.Namespace, p.Name)
			return nil
		},
	}
	nsFlag(rm)
	rm.Flags().BoolVar(&yes, "yes", false, "confirm the deletion")

	// No confirmation: it is undone by running it again (it goes forward), and
	// an agent asked to "roll back" should not have to know a flag for it.
	rollback := &cobra.Command{
		Use: "rollback NAME", Short: "Put the version before the running one back (run again to go forward)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, acct, err := o.connect()
			if err != nil {
				return err
			}
			p, err := o.find(cmd.Context(), c, acct, gns, args[0])
			if err != nil {
				return err
			}
			res, err := c.RollbackProject(cmd.Context(), acct, p.Namespace, p.Name)
			if err != nil {
				return o.explain(err)
			}
			if o.output == "json" {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s/%s: %s -> %s\n", p.Namespace, p.Name, res.From, res.To)
			// The two surprises: settings do not go back, and the next deploy
			// brings the newer version back.
			fmt.Fprintln(cmd.ErrOrStderr(), "only the code went back; env and other settings are unchanged. The next push or deploy replaces it.")
			return nil
		},
	}
	nsFlag(rollback)

	input := &cobra.Command{
		Use:   "input NAME [KEY=VALUE ...]",
		Short: "Show or supply the values qooq.yaml's inputs: asks for (setting them redeploys)",
		Long: `With only NAME, list the inputs the service's qooq.yaml declares and which
have a value. With KEY=VALUE pairs, set them and redeploy. KEY=@FILE reads the
value from a file, so a secret does not end up in your shell history; KEY= with
nothing after it clears that input. A secret's value is never shown back.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, acct, err := o.connect()
			if err != nil {
				return err
			}
			p, err := o.find(cmd.Context(), c, acct, gns, args[0])
			if err != nil {
				return err
			}
			if len(args) == 1 {
				if o.output == "json" {
					return writeJSON(cmd.OutOrStdout(), p.Inputs)
				}
				printProjectInputs(cmd.OutOrStdout(), p)
				return nil
			}
			values, err := parseInputArgs(args[1:])
			if err != nil {
				return err
			}
			updated, err := c.SetProjectInputs(cmd.Context(), acct, p.Namespace, p.Name, values)
			if err != nil {
				return o.explain(err)
			}
			if o.output == "json" {
				return writeJSON(cmd.OutOrStdout(), updated)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "saved; %s/%s is redeploying. Follow it with: qooq project get %s -n %s\n",
				p.Namespace, p.Name, p.Name, p.Namespace)
			return nil
		},
	}
	nsFlag(input)

	// Four verbs, not two toggles, for the same reason as the console:
	// stopping the service and stopping deploys are different decisions,
	// and each needs its one sentence about what it does not do.
	verb := func(use, short, done, note string, do func(*controlclient.Client, context.Context, string, string, string) error) *cobra.Command {
		c := &cobra.Command{
			Use: use + " NAME", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cl, acct, err := o.connect()
				if err != nil {
					return err
				}
				p, err := o.find(cmd.Context(), cl, acct, gns, args[0])
				if err != nil {
					return err
				}
				if err := do(cl, cmd.Context(), acct, p.Namespace, p.Name); err != nil {
					return o.explain(err)
				}
				if o.output == "json" {
					return writeJSON(cmd.OutOrStdout(), map[string]string{"namespace": p.Namespace, "name": p.Name, "done": use})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s/%s: %s\n", p.Namespace, p.Name, done)
				if note != "" {
					fmt.Fprintln(cmd.ErrOrStderr(), note)
				}
				return nil
			},
		}
		nsFlag(c)
		return c
	}
	stop := verb("stop", "Stop the running service (the project and its settings stay)", "stopped",
		"automatic deploys are still on: the next push starts it again. To keep it stopped, also run `qooq project pause`.",
		(*controlclient.Client).StopProject)
	start := verb("start", "Start a stopped service again", "started", "", (*controlclient.Client).StartProject)
	pause := verb("pause", "Stop deploying new pushes (what runs keeps running)", "automatic deploys paused",
		"`qooq project deploy` still deploys when asked; `qooq project resume` turns automatic deploys back on.",
		(*controlclient.Client).PauseProject)
	resume := verb("resume", "Deploy new pushes again", "automatic deploys resumed", "", (*controlclient.Client).ResumeProject)

	cmd.AddCommand(ls, create, get, deploy, logs, rm, rollback, input, stop, start, pause, resume)
	return cmd
}

// --- connection ---

// connect builds a client from the stored login. Not being signed in is the
// commonest failure here, so it gets its own sentence with the next command.
func (o *projectOpts) connect() (*controlclient.Client, string, error) {
	if o.output != "table" && o.output != "json" {
		return nil, "", fmt.Errorf("--output must be table or json, got %q", o.output)
	}
	addr := controlAddr(o.addr)
	login, ok, err := loadLogin(addr)
	if err != nil {
		return nil, "", err
	}
	if !ok || login.Token == "" {
		return nil, "", notSignedIn(addr)
	}
	acct := o.account
	if acct == "" {
		acct = login.Account
	}
	return controlclient.New(addr, controlclient.WithToken(login.Token), controlclient.WithUserAgent(cliUserAgent())), acct, nil
}

func notSignedIn(addr string) error {
	return fmt.Errorf("not signed in to %s: run `qooq auth login --control-addr %s`", addr, addr)
}

// explain turns a 401 into the next step; other errors pass through.
func (o *projectOpts) explain(err error) error {
	if controlclient.StatusCode(err) == http.StatusUnauthorized {
		addr := controlAddr(o.addr)
		return fmt.Errorf("the stored token for %s is not accepted (expired or revoked): run `qooq auth login --control-addr %s`", addr, addr)
	}
	// The server's sentence, without the route in front of it: "control PUT
	// /apis/v1/accounts/…/source: some_code: … (4xx)" buried the one
	// part a person (or an agent) acts on. The code stays, at the end, for
	// anything matching on it.
	var api *controlclient.APIError
	if errors.As(err, &api) && api.Message != "" {
		msg, code := api.Message, ""
		if i := strings.Index(msg, ": "); i > 0 && !strings.Contains(msg[:i], " ") {
			code, msg = msg[:i], msg[i+2:]
		}
		if code != "" {
			return fmt.Errorf("%s (%s)", msg, code)
		}
		return errors.New(msg)
	}
	return err
}

// find resolves NAME (and optional namespace) to one project.
func (o *projectOpts) find(ctx context.Context, c *controlclient.Client, acct, ns, name string) (controlclient.Project, error) {
	ps, err := c.ListProjects(ctx, acct)
	if err != nil {
		return controlclient.Project{}, o.explain(err)
	}
	return pickProject(ps, ns, name)
}

func pickProject(ps []controlclient.Project, ns, name string) (controlclient.Project, error) {
	var hits []controlclient.Project
	for _, p := range ps {
		if p.Name == name && (ns == "" || p.Namespace == ns) {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		if ns != "" {
			return controlclient.Project{}, fmt.Errorf("no service %q in namespace %q", name, ns)
		}
		return controlclient.Project{}, fmt.Errorf("no service named %q", name)
	}
	var nss []string
	for _, p := range hits {
		nss = append(nss, p.Namespace)
	}
	sort.Strings(nss)
	return controlclient.Project{}, fmt.Errorf("%q exists in several namespaces (%s): pass --namespace", name, strings.Join(nss, ", "))
}

// --- waiting ---

// newBuildDone reports whether cur describes a build that started after base
// was read and has finished. The status alone cannot say that: between the
// trigger and the build worker picking the target up, the project still reads
// as the previous deploy ("success"). A new build is proven by build_generation
// going up, or by last_build_at moving past the baseline.
func newBuildDone(base, cur controlclient.Project) bool {
	if cur.LastBuildStatus == statusBuilding || cur.LastBuildStatus == statusNever || cur.LastBuildStatus == "" {
		return false
	}
	if cur.BuildGeneration > base.BuildGeneration {
		return true
	}
	if cur.LastBuildAt != nil && (base.LastBuildAt == nil || cur.LastBuildAt.After(*base.LastBuildAt)) {
		return true
	}
	return false
}

// waitAndReport polls until the deploy that follows base finishes, then
// reports. The returned error carries a non-zero exit for failure outcomes.
func (o *projectOpts) waitAndReport(cmd *cobra.Command, c *controlclient.Client, acct string, base controlclient.Project, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()
	errw := cmd.ErrOrStderr()
	last := ""
	for {
		ps, err := c.ListProjects(ctx, acct)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return o.explain(err)
		}
		var cur *controlclient.Project
		for i := range ps {
			if ps[i].Name == base.Name && ps[i].Namespace == base.Namespace {
				cur = &ps[i]
			}
		}
		if cur == nil {
			return fmt.Errorf("%s/%s disappeared while waiting", base.Namespace, base.Name)
		}
		if cur.LastBuildStatus != last {
			last = cur.LastBuildStatus
			fmt.Fprintf(errw, "status: %s\n", last)
		}
		if newBuildDone(base, *cur) {
			return o.finish(cmd, c, acct, *cur)
		}
		select {
		case <-ctx.Done():
		case <-time.After(pollEvery):
			continue
		}
		break
	}
	return fmt.Errorf("timed out after %s waiting for %s/%s: no finished new build observed (last seen status %q)", timeout, base.Namespace, base.Name, last)
}

func (o *projectOpts) finish(cmd *cobra.Command, c *controlclient.Client, acct string, p controlclient.Project) error {
	out, errw := cmd.OutOrStdout(), cmd.ErrOrStderr()
	switch p.LastBuildStatus {
	case statusSuccess:
		if err := o.print(out, p); err != nil {
			return err
		}
		if len(p.Endpoints) == 0 {
			fmt.Fprintln(errw, "deployed: no public URL")
		} else {
			fmt.Fprintln(errw, "deployed: "+strings.Join(p.Endpoints, ", "))
		}
		if !p.Stopped && (p.Phase == "Failed" || p.Phase == "Exited") {
			return fmt.Errorf("the build succeeded but the service is %s%s", p.Phase, suffix(": ", p.PhaseError))
		}
		return nil
	case statusNeedsInput:
		_ = o.print(out, p)
		key := "KEY"
		if len(p.MissingInputs) > 0 {
			key = p.MissingInputs[0]
		}
		return fmt.Errorf("needs input: qooq.yaml declares values that have no answer yet (%s). Supply them with `qooq project input %s -n %s %s=<value>` (or in the console); that redeploys",
			orDash(p.LastBuildError), p.Name, p.Namespace, key)
	default:
		_ = o.print(out, p)
		// The full message is already on the last_error row above; this is
		// the one-line summary, said once. The build service's sentence
		// usually starts "build failed:" itself, and one about a version
		// that built but did not start is not a build failure.
		msg, _, _ := strings.Cut(orDash(p.LastBuildError), "\n")
		text, err := c.BuildLog(cmd.Context(), acct, p.Namespace, p.Name)
		built := err == nil && strings.Contains(text, "Successfully built")
		if !strings.HasPrefix(msg, "build ") {
			if built {
				msg = "deploy failed: " + msg
			} else {
				msg = "build " + p.LastBuildStatus + ": " + msg
			}
		}
		fmt.Fprintln(errw, msg)
		switch {
		case built && strings.Contains(p.LastBuildError, "--- last output"):
			// The message above already carries what the app printed.
		case built:
			// The image was built; it is the app that did not stay up. Its
			// own output says why — the build's would only show success.
			if rc, err := c.AppLogs(cmd.Context(), acct, p.Namespace, p.Name, 30, false); err == nil {
				b, _ := io.ReadAll(io.LimitReader(rc, 64<<10))
				rc.Close()
				if strings.TrimSpace(string(b)) != "" {
					fmt.Fprintf(errw, "--- the build succeeded; last lines the app printed ---\n%s\n", tailLines(string(b), 30))
				}
			}
		case err == nil && strings.TrimSpace(text) != "":
			fmt.Fprintf(errw, "--- last lines of the build log ---\n%s\n", tailLines(text, 30))
		}
		return fmt.Errorf("deploy of %s/%s failed", p.Namespace, p.Name)
	}
}

// printProjectInputs lists the declared inputs as a table.
func printProjectInputs(w io.Writer, p controlclient.Project) {
	if len(p.Inputs) == 0 {
		fmt.Fprintln(w, "qooq.yaml declares no inputs for this service")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tREQUIRED\tSET\tVALUE\tDESCRIPTION")
	for _, in := range p.Inputs {
		val := in.Value
		if in.Secret {
			val = "(secret)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", in.Name, yesNo(in.Required), yesNo(in.Set), orDash(val), in.Description)
	}
	_ = tw.Flush()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// parseInputArgs reads KEY=VALUE pairs; KEY=@FILE takes the value from a file
// (trailing newline dropped, as an editor leaves one).
func parseInputArgs(args []string) (map[string]string, error) {
	out := map[string]string{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%q is not KEY=VALUE", a)
		}
		if strings.HasPrefix(v, "@") {
			b, err := os.ReadFile(v[1:])
			if err != nil {
				return nil, fmt.Errorf("read the value of %s: %w", k, err)
			}
			v = strings.TrimRight(string(b), "\r\n")
		}
		out[k] = v
	}
	return out, nil
}

// ansiEscape matches terminal colour codes, which a build's output carries.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(ansiEscape.ReplaceAllString(s, ""), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func suffix(sep, s string) string {
	if s == "" {
		return ""
	}
	return sep + s
}

// --- output ---

// print writes one project: JSON for -o json, a one-row-per-field view else.
func (o *projectOpts) print(w io.Writer, p controlclient.Project) error {
	if o.output == "json" {
		return writeJSON(w, p)
	}
	return writeDetail(w, p)
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func filterNS(ps []controlclient.Project, ns string) []controlclient.Project {
	var out []controlclient.Project
	for _, p := range ps {
		if p.Namespace == ns {
			out = append(out, p)
		}
	}
	return out
}

func sortProjects(ps []controlclient.Project) {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].Namespace != ps[j].Namespace {
			return ps[i].Namespace < ps[j].Namespace
		}
		return ps[i].Name < ps[j].Name
	})
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func shortCommit(rev string) string {
	rev = strings.TrimPrefix(rev, "sha256:")
	if len(rev) > 7 {
		rev = rev[:7]
	}
	return orDash(rev)
}

// revisionText is what was deployed, in the form that says what kind of thing
// it is. An image service's revision is the image digest, not a git sha:
// printed bare, "785835e" reads as a commit somebody can look up in the repo.
//
// For a git service it is the commit that is running. When the last build was
// a different commit — it failed and was rolled back, or it is still building —
// that one follows in brackets: printing only the last build said a broken
// commit was live while the previous one was serving.
func revisionText(p controlclient.Project) string {
	if p.Source == "image" && p.LastBuildRevision != "" {
		return "sha256:" + shortCommit(p.LastBuildRevision)
	}
	running := runningCommit(p)
	s := shortCommit(running)
	if other := otherBuild(p, running); other != "" {
		s += " (" + other + ")"
	}
	return s
}

// runningCommit is the deployed commit, or the last build's when the control
// plane does not report one (older servers, or nothing has succeeded yet).
func runningCommit(p controlclient.Project) string {
	if p.DeployedRevision != "" {
		return p.DeployedRevision
	}
	return p.LastBuildRevision
}

// otherBuild describes the last build when it is not what is running.
func otherBuild(p controlclient.Project, running string) string {
	if p.LastBuildRevision == "" || p.LastBuildRevision == running {
		return ""
	}
	switch p.LastBuildStatus {
	case "failed":
		return shortCommit(p.LastBuildRevision) + " failed"
	case "building":
		return "building " + shortCommit(p.LastBuildRevision)
	}
	return ""
}

func fullDigest(rev string) string {
	if rev == "" || strings.HasPrefix(rev, "sha256:") {
		return rev
	}
	return "sha256:" + rev
}

func statusText(p controlclient.Project) string {
	s := orDash(p.LastBuildStatus)
	switch {
	case p.Paused:
		s += " (gave up)"
	case p.DeployPaused:
		s += " (paused)"
	}
	return s
}

func phaseText(p controlclient.Project) string {
	if p.Stopped {
		return "Stopped"
	}
	return orDash(p.Phase)
}

// pendingURL marks the address the platform gave a service that is not
// answering there yet: before its first deploy it does not open, and a bare
// URL would read as broken.
const pendingURL = " (available once deployed)"

func writeTable(w io.Writer, ps []controlclient.Project) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tNAME\tSTATUS\tPHASE\tURL\tREVISION")
	for _, p := range ps {
		url := "-"
		if len(p.Endpoints) > 0 {
			url = p.Endpoints[0]
		} else if p.URL != "" {
			url = p.URL + pendingURL
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", p.Namespace, p.Name, statusText(p), phaseText(p), url, revisionText(p))
	}
	return tw.Flush()
}

func writeDetail(w io.Writer, p controlclient.Project) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(tw, "%s:\t%s\n", k, v)
		}
	}
	ts := func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return ""
		}
		return t.Local().Format(time.RFC3339)
	}
	row("name", p.Name)
	row("namespace", p.Namespace)
	row("source", p.Source)
	row("repo", p.RepoURL)
	isImage := p.Source == "image"
	if !isImage && p.Source != "upload" {
		// An image or upload service has no branch; the "main" it carries is a default.
		row("branch", p.Branch)
	}
	if p.Source == "upload" {
		row("last_upload_at", func() string {
			if p.LastUploadAt == nil {
				return ""
			}
			return p.LastUploadAt.Local().Format(time.RFC3339)
		}())
	}
	row("image", p.ImageRef)
	row("status", statusText(p))
	row("phase", phaseText(p))
	row("phase_error", p.PhaseError)
	if isImage {
		// In full: "sha256:785835e" looks like a digest reference but cannot
		// be pulled, and pinning a version is what a digest is for. The list
		// abbreviates; the detail view has the room.
		row("digest", fullDigest(p.LastBuildRevision))
	} else {
		running := runningCommit(p)
		row("commit", shortCommit(running))
		if other := otherBuild(p, running); other != "" {
			row("last_build", other)
		}
	}
	row("last_build_at", ts(p.LastBuildAt))
	row("build_started_at", ts(p.BuildStartedAt))
	row("last_error", p.LastBuildError)
	if len(p.Endpoints) == 0 && p.URL != "" {
		row("url", p.URL+pendingURL)
	} else if len(p.Endpoints) == 0 {
		row("url", "no public URL")
	} else {
		for i, e := range p.Endpoints {
			k := "url"
			if i > 0 {
				k = ""
			}
			fmt.Fprintf(tw, "%s:\t%s\n", k, e)
		}
	}
	for i, s := range p.Services {
		k := ""
		if i == 0 {
			k = "services"
		}
		if s.Dropped {
			fmt.Fprintf(tw, "%s:\t%s (%s %s) NOT IN qooq.yaml: stopped, data kept until you delete it in the console (or add it back)\n", k, s.Name, s.Type, s.Version)
			continue
		}
		fmt.Fprintf(tw, "%s:\t%s (%s %s, %s)\n", k, s.Name, s.Type, s.Version, orDash(s.Phase))
	}
	if len(p.References) > 0 {
		row("references", strings.Join(p.References, ", "))
	}
	return tw.Flush()
}
