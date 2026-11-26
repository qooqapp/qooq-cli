package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// `qooq up` starts the daemons for this login session. That is the right thing
// while someone is sitting in front of the machine and the wrong thing for a
// machine that reboots: the daemons stay down, every component with them, and
// nothing says so until somebody notices their app is gone.
//
// This installs the same two daemons as a user service — launchd on macOS,
// systemd --user on Linux — so they come back on their own. The unit is
// generated from the same daemon definitions `qooq up` uses rather than being
// a file the reader is asked to copy and adjust: two descriptions of how to
// start qooqd would disagree the first time a flag changed, and the one nobody
// runs day to day is the one that would be wrong.

// serviceLabel is the launchd label / systemd unit stem for a daemon.
//
// Reverse-DNS on macOS because launchd labels share one namespace across
// everything a user runs; plain names on Linux because systemd unit files are
// already scoped by directory and `systemctl --user status qooqd` is what
// someone would type.
func serviceLabel(d daemon) string {
	if runtime.GOOS == "darwin" {
		return "app.qooq." + d.name
	}
	return d.name
}

func serviceUnitPath(d daemon) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine your home directory: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", serviceLabel(d)+".plist"), nil
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", d.name+".service"), nil
	default:
		return "", fmt.Errorf("qooq service is not supported on %s; run `qooq up` from a login script instead", runtime.GOOS)
	}
}

// serviceEnv is the environment a unit needs spelled out.
//
// A service does not inherit the shell's environment, which is where the one
// genuinely surprising failure lives: DOCKER_HOST. OrbStack, Colima, Rancher
// Desktop and podman put the socket somewhere non-standard and record it in a
// docker *context*, which the CLI reads and the daemons' Go SDK does not. In a
// terminal `qooq up` works because checkDocker resolves it every time; at boot
// there is nobody to resolve it, so it is baked in here.
func serviceEnv(home string, d daemon, dockerEndpoint string) []string {
	var env []string
	if dockerEndpoint != "" && d.name == "qooqd" {
		env = append(env, "DOCKER_HOST="+dockerEndpoint)
	}
	// PATH is not inherited either, and the watcher shells out to build
	// commands. A conservative default beats an empty one.
	env = append(env, "PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin")
	env = append(env, "QOOQ_HOME="+home)
	if d.env != nil {
		env = append(env, d.env(home)...)
	}
	return env
}

func launchdPlist(home string, d daemon, bin string, dockerEndpoint string) string {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	b.WriteString("  <key>Label</key>\n  <string>" + esc(serviceLabel(d)) + "</string>\n")

	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, a := range append([]string{bin}, d.args(home)...) {
		b.WriteString("    <string>" + esc(a) + "</string>\n")
	}
	b.WriteString("  </array>\n")

	b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
	for _, kv := range serviceEnv(home, d, dockerEndpoint) {
		k, v, _ := strings.Cut(kv, "=")
		b.WriteString("    <key>" + esc(k) + "</key>\n    <string>" + esc(v) + "</string>\n")
	}
	b.WriteString("  </dict>\n")

	b.WriteString("  <key>WorkingDirectory</key>\n  <string>" + esc(home) + "</string>\n")
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("  <key>KeepAlive</key>\n  <true/>\n")
	// qooqd exits when docker is unreachable, which at boot it usually is for
	// the first few seconds — Docker Desktop and OrbStack start alongside this,
	// not before it. Ten seconds between attempts turns that race into a
	// slightly late start instead of a service that gave up.
	b.WriteString("  <key>ThrottleInterval</key>\n  <integer>10</integer>\n")
	b.WriteString("  <key>StandardOutPath</key>\n  <string>" + esc(logFile(home, d)) + "</string>\n")
	b.WriteString("  <key>StandardErrorPath</key>\n  <string>" + esc(logFile(home, d)) + "</string>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func esc(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// envFile, when set, becomes EnvironmentFile= — after the Environment= lines,
// and systemd lets the file's values win over them. It is how a qooq Cloud node gets
// the settings this command knows nothing about (the two watcher tokens,
// untrusted_repos, the container and build limits) without anyone editing a
// generated file that the next install would overwrite.
func systemdUnit(home string, d daemon, bin string, dockerEndpoint, envFile string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Unit]\nDescription=%s (qooq)\nAfter=network-online.target\nWants=network-online.target\n\n", d.name)
	b.WriteString("[Service]\nType=simple\n")
	fmt.Fprintf(&b, "ExecStart=%s\n", systemdArgs(append([]string{bin}, d.args(home)...)))
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", home)
	for _, kv := range serviceEnv(home, d, dockerEndpoint) {
		fmt.Fprintf(&b, "Environment=%s\n", systemdValue(kv))
	}
	if envFile != "" {
		fmt.Fprintf(&b, "EnvironmentFile=%s\n", systemdValue(envFile))
	}
	// Same reasoning as launchd's ThrottleInterval: docker may not be up yet.
	b.WriteString("Restart=always\nRestartSec=10\n\n")
	b.WriteString("[Install]\nWantedBy=default.target\n")
	return b.String()
}

// systemdArgs renders a command line for ExecStart, quoting each argument.
func systemdArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, `"`+systemdValue(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a))+`"`)
	}
	return strings.Join(quoted, " ")
}

// systemdValue escapes systemd's one active character. A bare % starts a
// specifier and would be silently replaced by something else entirely.
func systemdValue(s string) string { return strings.ReplaceAll(s, "%", "%%") }

func serviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install qooq as a service so it starts on boot",
		Long: `Installs qooqd and qooq-watcher as user services: launchd on macOS,
systemd --user on Linux. They start at login and restart if they exit, so a
reboot does not quietly take every deployed component down with it.

State stays where ` + "`qooq up`" + ` puts it (~/.qooq, or QOOQ_HOME).`,
	}
	cmd.AddCommand(serviceInstallCmd(), serviceUninstallCmd(), serviceStatusCmd())
	return cmd
}

func serviceInstallCmd() *cobra.Command {
	var (
		noWatcher     bool
		ingress       bool
		ingressTLS    bool
		ingressIssuer string
		extra         []string
		envFile       string
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Write and start the service units",
		Long: `Generates the unit files from the same definitions ` + "`qooq up`" + ` uses and
starts them.

The daemons must not already be running: run ` + "`qooq down`" + ` first, or the
service will start a second copy that cannot bind the ports.

On a Linux server with no graphical login, user services stop when you log out
unless lingering is enabled:  loginctl enable-linger $USER`,
		RunE: func(_ *cobra.Command, _ []string) error {
			home, err := qooqHome()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(home, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", home, err)
			}

			if envFile != "" {
				// launchd has no EnvironmentFile; a plist that silently
				// ignored the file would be a node without its settings.
				if runtime.GOOS == "darwin" {
					return fmt.Errorf("--env-file is for systemd (Linux); on macOS put the variables in the shell that runs `qooq up`")
				}
				abs, err := filepath.Abs(envFile)
				if err != nil {
					return err
				}
				if _, err := os.ReadFile(abs); err != nil {
					return fmt.Errorf("--env-file: %w", err)
				}
				envFile = abs
			}

			cfg, err := loadLocalConfig(home)
			if err != nil {
				return err
			}
			port := cfg.ingressPort()
			if ingress && port == 0 {
				port = defaultIngressPort
			}
			// The same settings `qooq up` uses, so a machine started by the
			// service behaves like one started by hand.
			extra = append(cfg.qooqdArgs(port), extra...)
			if port > 0 && ingressTLS {
				extra = append(extra, "-ingress-tls")
				if ingressIssuer != "" {
					extra = append(extra, "-ingress-tls-issuer", ingressIssuer)
				}
			}

			daemons := []daemon{qooqdDaemon(extra)}
			if !noWatcher {
				daemons = append(daemons, watcherDaemon())
			}

			// A running daemon and a service that is about to start one both
			// want the same port. Whichever loses fails in a way that looks
			// like the install failed, so the conflict is refused up front.
			for _, d := range daemons {
				if listening(d.port) {
					return fmt.Errorf("%s is already running on :%d — run `qooq down` first, then install the service", d.name, d.port)
				}
			}

			// Resolved now rather than at boot, when there is no shell to ask.
			// Docker being down is not a reason to refuse: installing a boot
			// service before starting docker is a perfectly ordinary order to
			// do things in.
			dockerEndpoint := dockerEndpointForService()
			if dockerEndpoint == "" {
				fmt.Println("note: could not read a docker endpoint from the current context;")
				fmt.Println("      the service will use the default socket. If qooqd cannot reach")
				fmt.Println("      docker after a reboot, re-run this with docker running.")
			}

			for _, d := range daemons {
				bin, err := findDaemonBinary(d.binary)
				if err != nil {
					return err
				}
				if bin, err = filepath.Abs(bin); err != nil {
					return err
				}
				path, err := serviceUnitPath(d)
				if err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
				}

				var content string
				if runtime.GOOS == "darwin" {
					content = launchdPlist(home, d, bin, dockerEndpoint)
				} else {
					content = systemdUnit(home, d, bin, dockerEndpoint, envFile)
				}
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					return fmt.Errorf("write %s: %w", path, err)
				}
				fmt.Printf("wrote    %s\n", path)

				if err := serviceLoad(d); err != nil {
					return err
				}
			}

			// Never report a start that did not happen: the whole point of this
			// command is that nobody will be watching the next time.
			for _, d := range daemons {
				if !waitHealthy(d, 60*time.Second) {
					return fmt.Errorf("%s did not become healthy within 60s\n--- last lines of %s ---\n%s",
						d.name, logFile(home, d), tailFile(logFile(home, d), 15))
				}
				fmt.Printf("running  %-14s :%d\n", d.name, d.port)
			}

			fmt.Printf("\nThe daemons now start at login and restart if they exit.\n")
			fmt.Printf("state:  %s\n", home)
			if runtime.GOOS == "linux" {
				fmt.Println("\nOn a server you log into over SSH, keep them running after logout:")
				fmt.Println("  loginctl enable-linger $USER")
			}
			fmt.Println("\nremove with:  qooq service uninstall")
			return nil
		},
	}
	cmd.Flags().StringVar(&envFile, "env-file", "", "Linux only: an environment file both units load (EnvironmentFile=); its values win over the generated ones. For a qooq Cloud node: the watcher tokens, untrusted_repos, container and build limits")
	cmd.Flags().BoolVar(&noWatcher, "no-watcher", false, "install only qooqd, without the GitOps watcher")
	cmd.Flags().BoolVar(&ingress, "ingress", false, "run the reverse proxy so components get hostnames instead of ports (binds port 80)")
	cmd.Flags().BoolVar(&ingressTLS, "ingress-tls", false, "with --ingress, serve HTTPS (see --ingress-tls-issuer for which certificates)")
	cmd.Flags().StringVar(&ingressIssuer, "ingress-tls-issuer", "", `who signs the certificates: "acme" (a public CA) or "internal" (Caddy own CA)`)
	cmd.Flags().StringArrayVar(&extra, "qooqd-arg", nil, "extra argument to pass through to qooqd (repeatable)")
	return cmd
}

func serviceUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the services and remove their unit files",
		Long: `Stops both daemons and deletes the unit files.

Nothing under ~/.qooq is touched: databases, deployed components and running
containers all survive. Start the daemons again with ` + "`qooq up`" + `.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			found := false
			for _, d := range []daemon{qooqdDaemon(nil), watcherDaemon()} {
				path, err := serviceUnitPath(d)
				if err != nil {
					return err
				}
				if _, err := os.Stat(path); err != nil {
					continue
				}
				found = true
				if err := serviceUnload(d); err != nil {
					fmt.Fprintf(os.Stderr, "warning: %v\n", err)
				}
				if err := os.Remove(path); err != nil {
					return fmt.Errorf("remove %s: %w", path, err)
				}
				fmt.Printf("removed  %s\n", path)
			}
			if runtime.GOOS == "linux" && found {
				_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
			}
			if !found {
				fmt.Println("no qooq service units are installed")
				return nil
			}
			fmt.Println("\nYour data is untouched. Start the daemons again with: qooq up")
			return nil
		},
	}
}

func serviceStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the services are installed and answering",
		RunE: func(_ *cobra.Command, _ []string) error {
			for _, d := range []daemon{qooqdDaemon(nil), watcherDaemon()} {
				path, err := serviceUnitPath(d)
				if err != nil {
					return err
				}
				state := "not installed"
				if _, err := os.Stat(path); err == nil {
					// Installed and answering are different questions, and
					// only the second one means anything to the operator.
					if healthy(d) {
						state = "installed, healthy"
					} else if listening(d.port) {
						state = "installed, port held but not answering"
					} else {
						state = "installed, not running"
					}
				}
				fmt.Printf("%-14s %s\n", d.name, state)
			}
			return nil
		},
	}
}

// dockerEndpointForService resolves the docker endpoint without failing when
// docker is down — unlike checkDocker, which is right to refuse for `qooq up`
// (there is no point starting a daemon that will exit) and wrong here.
func dockerEndpointForService() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	out, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func serviceLoad(d daemon) error {
	path, err := serviceUnitPath(d)
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		target := fmt.Sprintf("gui/%d", os.Getuid())
		// Booting out first makes the command repeatable: re-installing over a
		// loaded unit otherwise fails with "service already loaded", which
		// reads like an error when it is the normal case for an upgrade.
		_ = exec.Command("launchctl", "bootout", target+"/"+serviceLabel(d)).Run()
		if out, err := exec.Command("launchctl", "bootstrap", target, path).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl bootstrap %s: %v\n%s", path, err, strings.TrimSpace(string(out)))
		}
	case "linux":
		if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl --user daemon-reload: %v\n%s", err, strings.TrimSpace(string(out)))
		}
		unit := d.name + ".service"
		if out, err := exec.Command("systemctl", "--user", "enable", "--now", unit).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl --user enable --now %s: %v\n%s", unit, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func serviceUnload(d daemon) error {
	switch runtime.GOOS {
	case "darwin":
		target := fmt.Sprintf("gui/%d/%s", os.Getuid(), serviceLabel(d))
		if out, err := exec.Command("launchctl", "bootout", target).CombinedOutput(); err != nil {
			msg := strings.TrimSpace(string(out))
			// "No such process" means it was not loaded, which is the state
			// this command is trying to reach.
			if strings.Contains(msg, "No such process") || msg == "" {
				return nil
			}
			return fmt.Errorf("launchctl bootout %s: %v\n%s", target, err, msg)
		}
	case "linux":
		unit := d.name + ".service"
		if out, err := exec.Command("systemctl", "--user", "disable", "--now", unit).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl --user disable --now %s: %v\n%s", unit, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func waitHealthy(d daemon, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if healthy(d) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}
