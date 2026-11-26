package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/qooqapp/qooq/pkg/client"
)

func componentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "component", Aliases: []string{"comp"}, Short: "Manage components"}
	cmd.AddCommand(deployCmd(), upsertCmd(), lsCmd(), getCmd(), startCmd(), stopCmd(), rmCmd(), statsCmd(), eventsCmd(), logsCmd(), rollbackCmd())
	return cmd
}

// deploySpecFlags holds the flag variables shared by `component deploy` and
// `component upsert` (upsert accepts every deploy flag: it is spec-for-spec
// the same request, just PUT instead of POST).
type deploySpecFlags struct {
	name, namespace, image, group, tarPath, buildDir, dockerfile, tag, restart, cpu, mem, domain, network string
	envs, ports, mounts, cmdArgs, healthExec                                                              []string
	healthHTTPPath                                                                                        string
	healthHTTPPort, healthInterval, healthTimeout, healthFailures, healthDelay                            int
}

// register wires every deploy-spec flag onto cmd.
func (f *deploySpecFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.name, "name", "", "component name (required)")
	cmd.Flags().StringVar(&f.namespace, "namespace", "", "namespace scoping the name; components in different namespaces may share a name")
	cmd.Flags().StringVar(&f.image, "image", "", "image ref, e.g. nginx:alpine")
	cmd.Flags().StringVar(&f.tarPath, "tar", "", "path to a `docker save` tarball to load and deploy")
	cmd.Flags().StringVar(&f.buildDir, "build", "", "directory with a Dockerfile to build and deploy")
	cmd.Flags().StringVar(&f.dockerfile, "dockerfile", "", "Dockerfile path relative to --build (default: Dockerfile)")
	cmd.Flags().StringVar(&f.tag, "tag", "", "tag to apply when building with --build")
	cmd.Flags().StringArrayVar(&f.cmdArgs, "cmd", nil, "override container command, one arg per flag, e.g. --cmd sh --cmd -c --cmd 'exit 1'")
	cmd.Flags().StringArrayVar(&f.envs, "env", nil, "environment variable KEY=VALUE (repeatable)")
	cmd.Flags().StringArrayVar(&f.ports, "port", nil, "port mapping [HOST:]CONTAINER[/proto], HOST may be \"auto\" to let the daemon pick (repeatable)")
	cmd.Flags().StringArrayVar(&f.mounts, "mount", nil, "bind mount HOSTPATH:CONTAINERPATH[:ro|rw], both absolute (repeatable)")
	cmd.Flags().StringVar(&f.network, "network", "", "container network to join; components on it reach each other by name (created if missing)")
	cmd.Flags().StringVar(&f.group, "group", "", "group name (auto-created if missing)")
	cmd.Flags().StringVar(&f.restart, "restart", "", `restart policy: "always" (default), "on-failure" or "no"`)
	cmd.Flags().StringVar(&f.cpu, "cpu", "", "CPU limit in cores, e.g. 0.5")
	cmd.Flags().StringVar(&f.mem, "mem", "", "memory limit, e.g. 64m, 1g")
	cmd.Flags().StringVar(&f.domain, "domain", "", "public hostname the component serves, e.g. app.example.com (declarative metadata in v1, not routed)")
	cmd.Flags().StringVar(&f.healthHTTPPath, "health-http", "", "HTTP health check path, e.g. /healthz (requires --health-port; mutually exclusive with --health-exec)")
	cmd.Flags().IntVar(&f.healthHTTPPort, "health-port", 0, "HTTP health check published host port")
	cmd.Flags().StringArrayVar(&f.healthExec, "health-exec", nil, "exec health check command, one arg per flag, e.g. --health-exec sh --health-exec -c --health-exec 'curl -f localhost'")
	cmd.Flags().IntVar(&f.healthInterval, "health-interval", 0, "health check probe interval in seconds (default 10)")
	cmd.Flags().IntVar(&f.healthTimeout, "health-timeout", 0, "health check probe timeout in seconds (default 5)")
	cmd.Flags().IntVar(&f.healthFailures, "health-failures", 0, "consecutive probe failures before restart (default 3)")
	cmd.Flags().IntVar(&f.healthDelay, "health-delay", 0, "delay before the first probe, in seconds (default 5)")
	_ = cmd.MarkFlagRequired("name")
}

// build resolves the image source (--image/--tar/--build, loading/building as
// needed) and assembles the wire DeployRequest.
func (f *deploySpecFlags) build(cmd *cobra.Command) (client.DeployRequest, error) {
	resolvedImage, err := resolveDeployImage(cmd, f.image, f.tarPath, f.buildDir, f.dockerfile, f.tag)
	if err != nil {
		return client.DeployRequest{}, err
	}
	envMap, err := parseEnv(f.envs)
	if err != nil {
		return client.DeployRequest{}, err
	}
	portSpecs, err := parsePorts(f.ports)
	if err != nil {
		return client.DeployRequest{}, err
	}
	mountSpecs, err := parseMounts(f.mounts)
	if err != nil {
		return client.DeployRequest{}, err
	}
	cpuLimit, err := parseCPULimit(f.cpu)
	if err != nil {
		return client.DeployRequest{}, err
	}
	memLimit, err := parseMemBytes(f.mem)
	if err != nil {
		return client.DeployRequest{}, err
	}
	healthCheck, err := buildHealthCheck(f.healthHTTPPath, f.healthHTTPPort, f.healthExec,
		f.healthInterval, f.healthTimeout, f.healthFailures, f.healthDelay)
	if err != nil {
		return client.DeployRequest{}, err
	}
	// --group names a group; auto-create it if it does not exist yet
	// (convenient CLI default). The wire GroupID is the group's id.
	groupID := ""
	if f.group != "" {
		g, err := resolveGroup(cmd, f.group, true)
		if err != nil {
			return client.DeployRequest{}, err
		}
		groupID = g.ID
	}
	return client.DeployRequest{
		Namespace:     f.namespace,
		Name:          f.name,
		Image:         resolvedImage,
		Command:       f.cmdArgs,
		Env:           envMap,
		Ports:         portSpecs,
		Mounts:        mountSpecs,
		Network:       f.network,
		GroupID:       groupID,
		RestartPolicy: f.restart,
		CPULimit:      cpuLimit,
		MemLimitBytes: memLimit,
		Domain:        f.domain,
		HealthCheck:   healthCheck,
	}, nil
}

func deployCmd() *cobra.Command {
	var f deploySpecFlags
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy a component from an image ref, a tarball, or a Dockerfile build context",
		RunE: func(cmd *cobra.Command, _ []string) error {
			req, err := f.build(cmd)
			if err != nil {
				return err
			}
			comp, err := newClient().Deploy(cmd.Context(), req)
			if err != nil {
				return err
			}
			fmt.Printf("deployed %s (%s) phase=%s container=%s\n", comp.Name, comp.ID, comp.Phase, short(comp.ContainerID))
			return nil
		},
	}
	f.register(cmd)
	return cmd
}

// upsertCmd is idempotent redeploy: create if the name is new, or replace the
// existing component's container in place (same id) if it already exists.
// Accepts every flag deployCmd does.
func upsertCmd() *cobra.Command {
	var f deploySpecFlags
	cmd := &cobra.Command{
		Use:   "upsert",
		Short: "Create or redeploy a component by name (idempotent; same id on replace)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			req, err := f.build(cmd)
			if err != nil {
				return err
			}
			comp, err := newClient().Upsert(cmd.Context(), req)
			if err != nil {
				return err
			}
			fmt.Printf("upserted %s (%s) phase=%s container=%s\n", comp.Name, comp.ID, comp.Phase, short(comp.ContainerID))
			return nil
		},
	}
	f.register(cmd)
	return cmd
}

// buildHealthCheck assembles a HealthCheckSpec from CLI flags. --health-http
// (+ --health-port) and --health-exec are mutually exclusive; neither set
// means no health check. Numeric flags left at 0 are sent as 0 and get
// server-side defaults (see core.normalizeHealthCheck).
func buildHealthCheck(httpPath string, httpPort int, execCmd []string, interval, timeout, failures, delay int) (*client.HealthCheckSpec, error) {
	hasHTTP := httpPath != "" || httpPort != 0
	hasExec := len(execCmd) > 0
	switch {
	case hasHTTP && hasExec:
		return nil, fmt.Errorf("--health-http/--health-port and --health-exec are mutually exclusive")
	case hasHTTP:
		if httpPath == "" || httpPort == 0 {
			return nil, fmt.Errorf("--health-http and --health-port must both be set")
		}
		return &client.HealthCheckSpec{
			Type: "http", HTTPPath: httpPath, HTTPPort: httpPort,
			IntervalSeconds: interval, TimeoutSeconds: timeout,
			FailureThreshold: failures, InitialDelaySeconds: delay,
		}, nil
	case hasExec:
		return &client.HealthCheckSpec{
			Type: "exec", ExecCommand: execCmd,
			IntervalSeconds: interval, TimeoutSeconds: timeout,
			FailureThreshold: failures, InitialDelaySeconds: delay,
		}, nil
	default:
		return nil, nil
	}
}

// parseCPULimit parses a CPU cores value such as "0.5" or "2". Empty means unset.
func parseCPULimit(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid --cpu %q: %w", s, err)
	}
	if f < 0 {
		return 0, fmt.Errorf("invalid --cpu %q: must not be negative", s)
	}
	return f, nil
}

// parseMemBytes parses a memory size such as "64m", "1g", "512k" or a plain
// byte count into bytes. Suffixes are 1024-based (b/k/m/g), matching Docker's
// --memory convention. Empty means unset.
func parseMemBytes(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	unit := int64(1)
	numPart := s
	if n := len(s); n > 0 {
		switch strings.ToLower(s[n-1:]) {
		case "b":
			numPart = s[:n-1]
		case "k":
			unit = 1024
			numPart = s[:n-1]
		case "m":
			unit = 1024 * 1024
			numPart = s[:n-1]
		case "g":
			unit = 1024 * 1024 * 1024
			numPart = s[:n-1]
		}
	}
	f, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid --mem %q: %w", s, err)
	}
	if f < 0 {
		return 0, fmt.Errorf("invalid --mem %q: must not be negative", s)
	}
	return int64(f * float64(unit)), nil
}

// resolveDeployImage picks exactly one of --image/--tar/--build and turns it
// into a concrete image ref/id, loading or building it first as needed.
func resolveDeployImage(cmd *cobra.Command, image, tarPath, buildDir, dockerfile, tag string) (string, error) {
	set := 0
	for _, v := range []string{image, tarPath, buildDir} {
		if v != "" {
			set++
		}
	}
	switch {
	case set == 0:
		return "", fmt.Errorf("one of --image, --tar, or --build is required")
	case set > 1:
		return "", fmt.Errorf("--image, --tar, and --build are mutually exclusive")
	case image != "":
		return image, nil
	case tarPath != "":
		res, err := loadTarball(cmd, tarPath)
		if err != nil {
			return "", err
		}
		if len(res.ImageIDs) == 0 {
			return "", fmt.Errorf("load %s: daemon reported no loaded images", tarPath)
		}
		return res.ImageIDs[0], nil
	default: // buildDir != ""
		res, err := buildFromDir(cmd, buildDir, dockerfile, tag)
		if err != nil {
			return "", err
		}
		return res.ImageID, nil
	}
}

func statsCmd() *cobra.Command {
	var watch bool
	cmd := &cobra.Command{
		Use:   "stats <id-or-name>",
		Short: "Show latest resource usage for a component",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := resolve(cmd, args[0])
			if err != nil {
				return err
			}
			if watch {
				// Redraw a clear-screen dashboard on every SSE sample until Ctrl-C.
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				err := newClient().WatchStats(ctx, c.ID, func(s client.StatsResponse) error {
					renderStatsDashboard(c.Name, c.ID, s)
					return nil
				})
				if ctx.Err() != nil { // clean exit on Ctrl-C
					fmt.Println()
					return nil
				}
				return err
			}
			s, err := newClient().Stats(cmd.Context(), c.ID)
			if err != nil {
				return err
			}
			fmt.Printf("component: %s\ncpu:       %.2f%%\nmem:       %d / %d bytes\nsampled:   %s\n",
				c.Name, s.CPUPercent, s.MemUsage, s.MemLimit, formatUTC(s.Time))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "stream live stats as a redrawing dashboard (Ctrl-C to stop)")
	return cmd
}

// humanBytes formats a byte count with 1024-based units (B, KiB, MiB, ...).
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// renderStatsDashboard clears the screen and draws a single-component stats box.
func renderStatsDashboard(name, id string, s client.StatsResponse) {
	shortID := id
	if len([]rune(shortID)) > 10 {
		shortID = string([]rune(shortID)[:9]) + "…"
	}
	title := fmt.Sprintf("%s (%s)", name, shortID)
	rows := []string{
		fmt.Sprintf("CPU      %.1f%%", s.CPUPercent),
		fmt.Sprintf("MEM      %s / %s", humanBytes(s.MemUsage), humanBytes(s.MemLimit)),
		fmt.Sprintf("UPDATED  %s", formatUTC(s.Time)),
	}
	inner := len([]rune(title)) + 1
	for _, r := range rows {
		if l := len([]rune(r)); l > inner {
			inner = l
		}
	}
	if inner < 27 {
		inner = 27
	}
	var b strings.Builder
	b.WriteString("\033[H\033[2J") // cursor home + clear screen
	b.WriteString("┌─ " + title + " " + strings.Repeat("─", inner-len([]rune(title))-1) + "┐\n")
	for _, r := range rows {
		b.WriteString("│ " + r + strings.Repeat(" ", inner-len([]rune(r))) + " │\n")
	}
	b.WriteString("└" + strings.Repeat("─", inner+2) + "┘\n")
	fmt.Print(b.String())
}

func eventsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "events <id-or-name>",
		Short: "List lifecycle/crash events for a component",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := resolve(cmd, args[0])
			if err != nil {
				return err
			}
			evs, err := newClient().Events(cmd.Context(), c.ID)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "TIME\tTYPE\tEXIT\tMESSAGE")
			for _, e := range evs {
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", formatUTC(e.Time), e.Type, e.ExitCode, e.Message)
			}
			return w.Flush()
		},
	}
}

func lsCmd() *cobra.Command {
	var sortBy string
	var reverse bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List components",
		RunE: func(cmd *cobra.Command, _ []string) error {
			comps, err := newClient().ListComponents(cmd.Context())
			if err != nil {
				return err
			}
			// Sorted by name: it is the leading column and the thing being
			// looked up. The id order the daemon returns is creation order,
			// which is meaningful to nobody reading this table.
			less, err := componentSorter(sortBy)
			if err != nil {
				return err
			}
			sort.SliceStable(comps, func(i, j int) bool {
				if reverse {
					return less(&comps[j], &comps[i])
				}
				return less(&comps[i], &comps[j])
			})
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			// NAMESPACE leads and NAME is bare: the two together are the
			// component's identity, and splitting them is what makes "which
			// stack is this one in" answerable by looking down one column. A
			// bare name is still a usable argument — `component get` only
			// refuses it when two namespaces really do hold that name, and
			// then it says which ones.
			fmt.Fprintln(w, "NAMESPACE\tNAME\tPHASE\tUPTIME\tIMAGE\tPORTS\tID")
			for _, c := range comps {
				// The id is a disambiguator, shortened here the way docker
				// shortens its own — `component get` takes any unambiguous
				// prefix, and prints the full value.
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					client.NormalizeNamespace(c.Namespace), c.Name, phaseWithAddrWarning(&c), formatUptime(c.Phase, c.StartedAt),
					c.Image, formatPorts(c.Ports), shortID(c.ID))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&sortBy, "sort", "name", "sort by: name (namespace then name), phase, started, image")
	cmd.Flags().BoolVarP(&reverse, "reverse", "r", false, "reverse the sort order")
	return cmd
}

// hasUptime reports whether a component has an uptime to show at all: the same
// condition formatUptime prints one under.
func hasUptime(c *client.ComponentResponse) bool {
	return c.Phase == "Running" && !c.StartedAt.IsZero()
}

// componentSorter returns the less function for a --sort field. A component
// with no start time (never started, or stopped) sorts last in either
// direction: "sort by started" is asked in order to see what is running, and
// pushing the ones that are not to the end answers that question directly.
func componentSorter(field string) (func(a, b *client.ComponentResponse) bool, error) {
	// Ordered by the two columns as printed rather than by the joined
	// "ns/name" string: joined, a namespaced "alpha/web" sorts between the
	// un-namespaced "api" and "web", which scatters each namespace's rows
	// through the table instead of grouping them.
	byName := func(a, b *client.ComponentResponse) bool {
		na, nb := client.NormalizeNamespace(a.Namespace), client.NormalizeNamespace(b.Namespace)
		if na != nb {
			return na < nb
		}
		return a.Name < b.Name
	}
	switch field {
	case "name", "namespace":
		return byName, nil
	case "phase":
		return func(a, b *client.ComponentResponse) bool {
			if a.Phase != b.Phase {
				return a.Phase < b.Phase
			}
			return byName(a, b)
		}, nil
	case "started", "uptime":
		return func(a, b *client.ComponentResponse) bool {
			// Ordered by the UPTIME column as printed, which is why a stopped
			// component counts as having none even though its record still
			// remembers when it last started — sorting by a value the table
			// shows as "-" looks like no ordering at all.
			ua, ub := hasUptime(a), hasUptime(b)
			if ua != ub {
				return ua
			}
			if !ua || a.StartedAt.Equal(b.StartedAt) {
				return byName(a, b)
			}
			return a.StartedAt.Before(b.StartedAt) // longest uptime first
		}, nil
	case "image":
		return func(a, b *client.ComponentResponse) bool {
			if a.Image != b.Image {
				return a.Image < b.Image
			}
			return byName(a, b)
		}, nil
	default:
		return nil, fmt.Errorf("unknown --sort field %q: use name, namespace, phase, started or image", field)
	}
}

func getCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <id-or-name>",
		Short: "Show a component",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := resolve(cmd, args[0])
			if err != nil {
				return err
			}
			fmt.Printf("id:                %s\nname:              %s\nimage:             %s\nphase:             %s\ncontainer:         %s\nexit_code:         %d\ncrash_count:       %d\ngroup:             %s\nnetwork:           %s\nports:             %s\nendpoints:         %s\nenv:               %s\ndesired_state:     %s\nrestart_policy:    %s\nrestart_count:     %d\nrestart_exhausted: %t\nprevious_image:    %s\ndomain:            %s\ncpu_limit:         %s\nmem_limit:         %d bytes\nlast_restart:      %s\nhealth_check:      %s\nstarted_at:        %s\nuptime:            %s\ncreated:           %s\n",
				c.ID, c.Name, c.Image, c.Phase, c.ContainerID, c.ExitCode, c.CrashCount, c.GroupID, dashIfEmpty(c.Network),
				formatPorts(c.Ports), formatEndpoints(c.Endpoints), formatEnv(c.Env),
				c.DesiredState, c.RestartPolicy, c.RestartCount, c.RestartExhausted, dashIfEmpty(c.PreviousImage), dashIfEmpty(c.Domain), formatCPULimit(c.CPULimit), c.MemLimitBytes,
				formatTimeOrDash(c.LastRestartAt), formatHealthCheck(c.HealthCheck), formatTimeOrDash(c.StartedAt), formatUptime(c.Phase, c.StartedAt), formatUTC(c.CreatedAt))
			// Mounts are usually absent and each one needs a whole line to
			// stay readable, so they get their own block instead of a field.
			if c.Namespace != "" {
				fmt.Printf("namespace:         %s\ncontainer_name:    %s\n", c.Namespace, c.ContainerName)
			}
			if len(c.Mounts) > 0 {
				fmt.Println("mounts:")
				for _, m := range c.Mounts {
					mode := "rw"
					if m.ReadOnly {
						mode = "ro"
					}
					fmt.Printf("  %s -> %s (%s)\n", m.HostPath, m.ContainerPath, mode)
				}
			}
			// Printed last and only when set: it is the first thing an operator
			// needs when phase is Failed, and noise otherwise.
			if c.LastError != "" {
				fmt.Printf("last_error:        %s\n", c.LastError)
			}
			// Both values, so nobody has to go and look up what the address is
			// now in order to understand what the mark in `ls` meant.
			if c.AdvertiseHostAtDeploy != "" {
				fmt.Printf("stale_address:     deployed with %s, this machine is now %s\n",
					c.AdvertiseHostAtDeploy, c.AdvertiseHostNow)
				fmt.Printf("                   its env still names the old one; redeploy to update it\n")
			}
			return nil
		},
	}
}

func startCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start <id-or-name>",
		Short: "Start a component",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := resolve(cmd, args[0])
			if err != nil {
				return err
			}
			out, err := newClient().StartComponent(cmd.Context(), c.ID)
			if err != nil {
				return err
			}
			fmt.Printf("started %s phase=%s\n", out.Name, out.Phase)
			return nil
		},
	}
}

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <id-or-name>",
		Short: "Stop a component",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := resolve(cmd, args[0])
			if err != nil {
				return err
			}
			out, err := newClient().StopComponent(cmd.Context(), c.ID)
			if err != nil {
				return err
			}
			fmt.Printf("stopped %s phase=%s\n", out.Name, out.Phase)
			return nil
		},
	}
}

func rollbackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rollback <id-or-name>",
		Short: "Roll back a component to its previous image (run again to undo)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := resolve(cmd, args[0])
			if err != nil {
				return err
			}
			out, err := newClient().RollbackComponent(cmd.Context(), c.ID)
			if err != nil {
				return err
			}
			fmt.Printf("rolled back %s to image %s (previous is now %s) phase=%s\n",
				out.Name, out.Image, out.PreviousImage, out.Phase)
			return nil
		},
	}
}

func rmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <id-or-name>",
		Aliases: []string{"remove"},
		Short:   "Remove a component",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := resolve(cmd, args[0])
			if err != nil {
				return err
			}
			if err := newClient().RemoveComponent(cmd.Context(), c.ID); err != nil {
				return err
			}
			fmt.Printf("removed %s (%s)\n", c.Name, c.ID)
			return nil
		},
	}
}

// resolve accepts either a component ULID or a name and returns the component.
func resolve(cmd *cobra.Command, ref string) (*client.ComponentResponse, error) {
	c := newClient()
	if comp, err := c.GetComponent(cmd.Context(), ref); err == nil {
		return comp, nil
	}
	comps, err := c.ListComponents(cmd.Context())
	if err != nil {
		return nil, err
	}

	// "ns/name" addresses one component exactly; a bare name may now match
	// several, since namespaces make names unique only within a namespace.
	wantNS, wantName, qualified := strings.Cut(ref, "/")
	if !qualified {
		wantName = ref
	}
	// Normalized on both sides: a daemon old enough to predate the stored
	// default still answers with an empty namespace, and a name printed in a
	// table that is rejected when typed back is worse than not printing it.
	wantNS = client.NormalizeNamespace(wantNS)

	var matches []*client.ComponentResponse
	for i := range comps {
		// An id prefix addresses one component directly, which is what makes
		// the shortened id in `component ls` usable as an argument. Checked
		// before the name match so a prefix is never mistaken for a name.
		if !qualified && len(ref) >= 4 && strings.HasPrefix(comps[i].ID, ref) {
			matches = append(matches, &comps[i])
			continue
		}
		if comps[i].Name != wantName {
			continue
		}
		if qualified && client.NormalizeNamespace(comps[i].Namespace) != wantNS {
			continue
		}
		matches = append(matches, &comps[i])
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no component with id or name %q", ref)
	case 1:
		return matches[0], nil
	default:
		// Picking one silently would act on an arbitrary component — the kind
		// of thing that is only noticed after it has already done something.
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, componentRefName(m))
		}
		sort.Strings(names)
		return nil, fmt.Errorf("%q is ambiguous, it matches %s — address one with namespace/name",
			ref, strings.Join(names, ", "))
	}
}

// shortID trims a ULID for display. The full value stays available from
// `component get`, and any unambiguous prefix works as an argument — so the
// column is short enough to scan without making the id unusable.
func shortID(id string) string {
	const n = 12
	if len(id) > n {
		return id[:n]
	}
	return id
}

// componentRefName is the unambiguous way to address a component:
// "namespace/name", always qualified, with the default namespace spelled out.
// The bare name still works as an argument whenever it matches only one
// component — this is the form to *print*, because it stays correct after a
// second namespace acquires the same name.
// phaseWithAddrWarning marks a component whose configuration still names an
// address this machine no longer has.
//
// It belongs in the phase column, in `ls`, because that is the first thing
// anyone runs when something is not working. The whole difficulty of this
// failure is that every other signal says fine — the container is up, the
// probe is green — so a mark that requires already suspecting the cause and
// running `get` would arrive far too late to be useful.
func phaseWithAddrWarning(c *client.ComponentResponse) string {
	if c.AdvertiseHostAtDeploy == "" {
		return c.Phase
	}
	return c.Phase + " (stale addr)"
}

func componentRefName(c *client.ComponentResponse) string {
	return client.NormalizeNamespace(c.Namespace) + "/" + c.Name
}

func parseEnv(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --env %q, want KEY=VALUE", p)
		}
		out[k] = v
	}
	return out, nil
}

// parsePorts parses "[HOST:]CONTAINER[/proto]" specs into PortSpecs.
// parseMounts turns --mount HOSTPATH:CONTAINERPATH[:ro|rw] into MountSpecs.
// Unlike a qooq.yaml's run.files/run.volumes, there is no repo to resolve
// against here, so both paths must already be absolute.
func parseMounts(specs []string) ([]client.MountSpec, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]client.MountSpec, 0, len(specs))
	for _, s := range specs {
		parts := strings.Split(s, ":")
		var ms client.MountSpec
		switch len(parts) {
		case 2:
			ms.HostPath, ms.ContainerPath = parts[0], parts[1]
		case 3:
			ms.HostPath, ms.ContainerPath = parts[0], parts[1]
			switch strings.ToLower(parts[2]) {
			case "ro":
				ms.ReadOnly = true
			case "rw":
			default:
				return nil, fmt.Errorf("invalid --mount %q: mode must be \"ro\" or \"rw\"", s)
			}
		default:
			return nil, fmt.Errorf("invalid --mount %q: expected HOSTPATH:CONTAINERPATH[:ro|rw]", s)
		}
		if !filepath.IsAbs(ms.HostPath) || !filepath.IsAbs(ms.ContainerPath) {
			return nil, fmt.Errorf("invalid --mount %q: both paths must be absolute", s)
		}
		out = append(out, ms)
	}
	return out, nil
}

func parsePorts(specs []string) ([]client.PortSpec, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]client.PortSpec, 0, len(specs))
	for _, s := range specs {
		proto := "tcp"
		mapping := s
		if base, p, ok := strings.Cut(s, "/"); ok {
			mapping, proto = base, p
		}
		var ps client.PortSpec
		ps.Protocol = proto
		if hostStr, contStr, ok := strings.Cut(mapping, ":"); ok {
			cont, err := strconv.Atoi(contStr)
			if err != nil {
				return nil, fmt.Errorf("invalid --port %q: %w", s, err)
			}
			// "auto:8080" hands the host side to the daemon, which is the only
			// party that can see what the rest of the machine already uses.
			if strings.EqualFold(hostStr, "auto") {
				ps.ContainerPort, ps.AutoHost = cont, true
				out = append(out, ps)
				continue
			}
			host, err := strconv.Atoi(hostStr)
			if err != nil {
				return nil, fmt.Errorf("invalid --port %q: host port must be a number or \"auto\": %w", s, err)
			}
			ps.HostPort, ps.ContainerPort = host, cont
		} else {
			cont, err := strconv.Atoi(mapping)
			if err != nil {
				return nil, fmt.Errorf("invalid --port %q: %w", s, err)
			}
			ps.ContainerPort = cont
		}
		out = append(out, ps)
	}
	return out, nil
}

func formatCPULimit(cores float64) string {
	if cores == 0 {
		return "-"
	}
	return strconv.FormatFloat(cores, 'g', -1, 64)
}

func formatHealthCheck(hc *client.HealthCheckSpec) string {
	if hc == nil {
		return "-"
	}
	switch hc.Type {
	case "http":
		return fmt.Sprintf("http %s:%d interval=%ds timeout=%ds failures=%d delay=%ds",
			hc.HTTPPath, hc.HTTPPort, hc.IntervalSeconds, hc.TimeoutSeconds, hc.FailureThreshold, hc.InitialDelaySeconds)
	case "exec":
		return fmt.Sprintf("exec %q interval=%ds timeout=%ds failures=%d delay=%ds",
			strings.Join(hc.ExecCommand, " "), hc.IntervalSeconds, hc.TimeoutSeconds, hc.FailureThreshold, hc.InitialDelaySeconds)
	default:
		return "-"
	}
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// formatPorts renders the port mappings as "host->container/proto" entries, or
// "container/proto (internal)" for a port with no published host port.
func formatPorts(ports []client.PortSpec) string {
	if len(ports) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		if p.HostPort != 0 {
			parts = append(parts, fmt.Sprintf("%d->%d/%s", p.HostPort, p.ContainerPort, proto))
		} else {
			parts = append(parts, fmt.Sprintf("%d/%s (internal)", p.ContainerPort, proto))
		}
	}
	return strings.Join(parts, ", ")
}

// formatEndpoints renders the ready-to-open URLs the daemon reported, so the
// operator never has to reach for `docker ps` to learn where an app is
// reachable.
//
// The daemon composes these, not the CLI: only qooqd knows the host's advertise
// address. This used to be built here from the port list with 127.0.0.1 hard
// coded, which was wrong the moment anyone wanted to open the app from their
// phone — the container was listening on 0.0.0.0 the whole time.
func formatEndpoints(endpoints []string) string {
	if len(endpoints) == 0 {
		return "-"
	}
	return strings.Join(endpoints, ", ")
}

// formatEnv renders env vars as sorted "k=v" pairs for stable output.
func formatEnv(env map[string]string) string {
	if len(env) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+env[k])
	}
	return strings.Join(parts, " ")
}

// formatUTC renders any timestamp the CLI displays in one uniform shape:
// UTC+0, second precision, "2006-01-02 15:04:05". Server responses carry
// full zone info; normalizing at display time keeps every command's output
// comparable regardless of daemon or terminal timezone.
func formatUTC(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

func formatTimeOrDash(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return formatUTC(t)
}

// formatUptime renders how long the current container instance has been up,
// or "-" if it isn't currently running (started_at is only meaningful while
// Running; a stopped/crashed component's last start time isn't "uptime").
// Precision scales with magnitude so the value stays readable: days+hours
// past a day, hours+minutes past an hour, minutes+seconds otherwise.
func formatUptime(phase string, startedAt time.Time) string {
	if phase != "Running" || startedAt.IsZero() {
		return "-"
	}
	d := time.Since(startedAt).Round(time.Second)
	if d < 0 {
		d = 0
	}
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	hours := d / time.Hour
	d -= hours * time.Hour
	minutes := d / time.Minute
	d -= minutes * time.Minute
	seconds := d / time.Second

	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm%ds", minutes, seconds)
	}
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
