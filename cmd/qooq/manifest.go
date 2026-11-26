package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/qooqapp/qooq/pkg/manifest"
)

// manifestCmd checks a qooq.yaml without deploying it. Like `skill`, it never
// talks to a daemon: the point is to answer before committing, and requiring a
// running watcher would put the check on the far side of the loop it exists to
// shorten — commit, push, poll, clone, build, and only then a parse error about
// a typo.
func manifestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "manifest", Short: "Work with an app manifest (qooq.yaml)"}
	cmd.AddCommand(manifestValidateCmd())
	return cmd
}

func manifestValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [file]",
		Short: "Check a qooq.yaml for errors, without a daemon",
		Long: "Parse and validate an app manifest locally.\n\n" +
			"With no argument it reads ./qooq.yaml; \"-\" reads standard input.\n" +
			"Exits non-zero when the manifest would fail to deploy, so it can\n" +
			"gate a commit hook or a CI step.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "qooq.yaml"
			if len(args) == 1 {
				path = args[0]
			}

			data, name, err := readManifest(cmd, path)
			if err != nil {
				return err
			}
			m, err := manifest.Parse(data)
			if err != nil {
				// The error already names the field or the yaml problem; the
				// file name is what a caller validating several needs added.
				return fmt.Errorf("%s: %w", name, err)
			}

			fmt.Printf("%s: ok\n", name)
			describeManifest(m)
			warnBusyPorts(cmd, m)
			return nil
		},
	}
}

func readManifest(cmd *cobra.Command, path string) (data []byte, name string, err error) {
	if path == "-" {
		data, err = io.ReadAll(cmd.InOrStdin())
		return data, "(stdin)", err
	}
	data, err = os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		abs, _ := filepath.Abs(path)
		return nil, path, fmt.Errorf("no manifest at %s — pass a path, or run this where your qooq.yaml is", abs)
	}
	return data, path, err
}

// describeManifest reports what the manifest asks for, so a passing check is
// still informative: "valid" and "says what I meant" are different questions,
// and the second one is the one a typo in a key silently gets wrong.
func describeManifest(m *manifest.Manifest) {
	line := func(label, value string) {
		if value != "" {
			fmt.Printf("  %-12s %s\n", label+":", value)
		}
	}
	line("app", m.App)
	line("domain", m.Domain)

	// Mounts come before the rest because they are the part most worth a human
	// glance. A volume mounted at the wrong path is how an app with a non-root
	// USER ends up unable to write its own data directory, and that failure
	// arrives as a container which exits with nothing in its logs.
	for _, mt := range m.Run.Volumes {
		line("volume", mountLine(mt))
	}
	for _, mt := range m.Run.Files {
		line("file", mountLine(mt))
	}
	line("network", m.Network)

	switch {
	case m.Build.Template != "":
		line("build", "template "+m.Build.Template)
	case m.Build.Dockerfile != "":
		line("build", m.Build.Dockerfile)
	case len(m.Build.Command) > 0:
		line("build", fmt.Sprintf("%v (process runtime)", m.Build.Command))
	}
	if len(m.Run.Ports) > 0 {
		line("ports", fmt.Sprintf("%v", m.Run.Ports))
	}
	if n := len(m.Services); n > 0 {
		names := make([]string, 0, n)
		for k := range m.Services {
			names = append(names, k)
		}
		line("services", fmt.Sprintf("%v", names))
	}
	if n := len(m.Inputs); n > 0 {
		var required []string
		for k, in := range m.Inputs {
			if in.Required {
				required = append(required, k)
			}
		}
		line("inputs", fmt.Sprintf("%d declared, %d required %v", n, len(required), required))
	}
	switch {
	case m.Health.HTTP != "":
		line("health", fmt.Sprintf("http %s on port %d", m.Health.HTTP, m.Health.Port))
	case len(m.Health.Exec) > 0:
		line("health", fmt.Sprintf("exec %v", m.Health.Exec))
	}
}

func mountLine(m manifest.Mount) string {
	s := m.From + " -> " + m.To
	if m.Mode != "" {
		s += " (" + m.Mode + ")"
	}
	return s
}

// warnBusyPorts reports host ports the manifest pins that something on this
// machine already holds.
//
// This is the earliest moment the collision can be known. Otherwise it is
// found by the container failing to start — after a clone and a full build,
// which for a front-end project is ten minutes of waiting to be told a number
// was taken. The check costs one listen.
//
// The port an already-deployed app is currently serving on is not a collision:
// redeploying it to the same port is the normal case, and qooq stops the old
// container first. Warning about that would fire on every healthy project,
// every time — and a warning that is usually wrong teaches people to skip
// reading warnings, including the true ones. So a port held by a qooq
// component is reported as who holds it, and only an unattributable holder is
// called a problem.
func warnBusyPorts(cmd *cobra.Command, m *manifest.Manifest) {
	busy := make(map[int]bool)
	for _, spec := range m.Run.Ports {
		p, err := manifest.ParsePort(spec)
		// Unparseable ports are the validator's business, and a port with no
		// host side publishes nothing to collide with.
		if err == nil && p.Host != 0 && hostPortBusy(p.Host) {
			busy[p.Host] = true
		}
	}
	if len(busy) == 0 {
		return
	}
	holders := componentsByHostPort(cmd)

	for _, spec := range m.Run.Ports {
		p, err := manifest.ParsePort(spec)
		if err != nil || p.Host == 0 || !busy[p.Host] {
			continue
		}
		if name, ok := holders[p.Host]; ok {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"  note:    host port %d is currently held by component %q — fine if that is this app.\n",
				p.Host, name)
			continue
		}
		fmt.Fprintf(cmd.ErrOrStderr(),
			"  warning: host port %d is in use on this machine by something qooq does not manage —\n"+
				"           deploying this manifest here would fail to start. Pick another number,\n"+
				"           or write \"auto:%d\" to let the daemon choose.\n",
			p.Host, p.Container)
	}
}

// componentsByHostPort asks the daemon which of its components hold which host
// ports. An unreachable daemon is not an error here — validation deliberately
// works without one — it only means no holder can be named, and every busy
// port is reported as unattributable.
func componentsByHostPort(cmd *cobra.Command) map[int]string {
	held := make(map[int]string)
	comps, err := newClient().ListComponents(cmd.Context())
	if err != nil {
		return held
	}
	for i := range comps {
		c := &comps[i]
		if c.Phase == "Removed" {
			continue
		}
		for _, p := range c.Ports {
			if p.HostPort != 0 {
				held[p.HostPort] = componentRefName(c)
			}
		}
	}
	return held
}

// hostPortBusy asks over IPv4 on 0.0.0.0 specifically. net.Listen("tcp",
// ":port") binds the IPv6 wildcard, which on macOS succeeds even while a
// docker port publisher holds the same port on IPv4 — so every published port
// on the machine would report as free, and this warning would never fire on
// the collisions it exists to catch.
func hostPortBusy(port int) bool {
	ln, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return true
	}
	_ = ln.Close()
	return false
}
