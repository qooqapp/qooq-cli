package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// qooq needs two long-running processes — qooqd and qooq-watcher — and until
// now starting them was the reader's problem: two terminals, two sets of
// flags, and a database written into whatever directory they happened to be
// standing in. `qooq up` is that knowledge written down once.
//
// This is the native-process path. `qooq compose` does the same job by running
// the daemons as containers, but it needs the compose files from the qooq
// repository — which someone who downloaded a release archive does not have.

// daemon describes one of the two processes this command manages.
type daemon struct {
	name string
	// binary is the executable name looked up on disk.
	binary string
	// port is where it listens; the health URL and "is it already running"
	// check are both derived from it.
	port int
	// healthPath differs between the two: qooqd serves its health endpoint
	// under /v1, the watcher (a weedbox service) at the root.
	healthPath string
	// args are appended to the command line. The watcher takes none: it is
	// configured entirely through the environment.
	args func(home string) []string
	// env adds to the inherited environment.
	env func(home string) []string
}

func qooqdDaemon(extraArgs []string) daemon {
	return daemon{
		name:       "qooqd",
		binary:     "qooqd",
		port:       7777,
		healthPath: "/v1/healthz",
		args: func(home string) []string {
			return append([]string{
				"-listen", "127.0.0.1:7777",
				"-data", filepath.Join(home, "qooq.db"),
				// qooqd's own default keeps the build cache forever, which on a
				// laptop deploying the same projects all day ends in a full
				// disk. A cap here, not in qooqd: a server operator sets theirs
				// deliberately. --qooqd-arg -build-cache-max=... overrides it
				// (the last value of a flag wins).
				"-build-cache-max", "10g",
			}, extraArgs...)
		},
	}
}

func watcherDaemon() daemon {
	return daemon{
		name:       "qooq-watcher",
		binary:     "qooq-watcher",
		port:       7778,
		healthPath: "/healthz",
		args:       func(string) []string { return nil },
		env: func(home string) []string {
			// The watcher creates its sqlite file relative to the working
			// directory. Left alone it would drop watcher.db into whatever
			// project the user ran this from, so the path is made explicit
			// rather than relying on where the process happens to start.
			return []string{
				"QOOQ_WATCHER_SQLITE_PATH=" + filepath.Join(home, "watcher.db"),
				"QOOQ_WATCHER_MOUNTS_DIR=" + filepath.Join(home, "mounts"),
				"QOOQ_WATCHER_ARTIFACTS_DIR=" + filepath.Join(home, "artifacts"),
				// Without it `qooq deploy .` archives land in an uploads/ under
				// whatever directory `qooq up` was run from.
				"QOOQ_WATCHER_UPLOADS_DIR=" + filepath.Join(home, "uploads"),
			}
		},
	}
}

// qooqHome is where all local state lives: databases, logs, pidfiles, and the
// directories the watcher materializes mounts and artifacts into.
func qooqHome() (string, error) {
	if h := os.Getenv("QOOQ_HOME"); h != "" {
		return h, nil
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine your home directory; set QOOQ_HOME: %w", err)
	}
	return filepath.Join(dir, ".qooq"), nil
}

// findDaemonBinary locates a daemon executable.
//
// Next to the qooq binary first: a release archive is normally extracted into
// one directory, and finding the sibling that shipped with this CLI is more
// predictable than whatever an unrelated PATH entry might offer.
//
// QOOQ_BIN_DIR overrides all of it, which is what makes this usable from a
// development checkout: there the CLI and the daemons are built into two
// different repositories' bin/ directories and are never siblings. Guessing a
// relative path between the two repos would be magic that breaks the moment
// someone lays their checkouts out differently, so it is stated instead.
func findDaemonBinary(name string) (string, error) {
	if dir := os.Getenv("QOOQ_BIN_DIR"); dir != "" {
		candidate := filepath.Join(dir, name)
		if isExecutable(candidate) {
			return candidate, nil
		}
		return "", fmt.Errorf("QOOQ_BIN_DIR is %s but %s is not there (or is not executable)", dir, name)
	}
	if self, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		candidate := filepath.Join(filepath.Dir(self), name)
		if isExecutable(candidate) {
			return candidate, nil
		}
	}
	if found, err := exec.LookPath(name); err == nil {
		return found, nil
	}
	if home, err := qooqHome(); err == nil {
		candidate := filepath.Join(home, "bin", name)
		if isExecutable(candidate) {
			return candidate, nil
		}
	}
	// Last: the sibling checkout. This is not a guess — go.mod's replace
	// directive points at ../qooq, so a source build *must* have the two
	// repositories side by side, which puts the daemons at
	// <...>/qooq/bin/ while this binary is at <...>/qooq-cli/bin/. Only
	// used when something executable is actually there, so a different
	// layout falls through to the error below rather than being misled.
	if self, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		candidate := filepath.Join(filepath.Dir(self), "..", "..", "qooq", "bin", name)
		if isExecutable(candidate) {
			return filepath.Clean(candidate), nil
		}
	}
	return "", fmt.Errorf("cannot find %s — it ships alongside this CLI in the qooq release archive.\n"+
		"Put it on your PATH or next to the qooq binary; from a source checkout, set QOOQ_BIN_DIR to the directory holding it (e.g. QOOQ_BIN_DIR=../qooq/bin)", name)
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// listening reports whether something already holds the port. Used both to
// skip starting a second copy and to confirm a stop actually took effect.
func listening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func healthy(d daemon) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d%s", d.port, d.healthPath))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func pidFile(home string, d daemon) string { return filepath.Join(home, d.name+".pid") }
func logFile(home string, d daemon) string { return filepath.Join(home, d.name+".log") }

func upCmd() *cobra.Command {
	var (
		noWatcher     bool
		ingress       bool
		ingressTLS    bool
		ingressIssuer string
		extra         []string
	)
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start qooqd and qooq-watcher in the background",
		Long: `Starts the two daemons qooq needs, with their state under ~/.qooq
(override with QOOQ_HOME): databases, logs and the directories the watcher
materializes mounts and artifacts into.

Already-running daemons are left alone, so it is safe to run repeatedly.

This runs them as ordinary processes. ` + "`qooq compose`" + ` does the same job with
containers instead, but needs the compose files from the qooq repository.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := qooqHome()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(home, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", home, err)
			}

			cfg, err := loadLocalConfig(home)
			if err != nil {
				return err
			}
			port := cfg.ingressPort()
			if ingress && port == 0 {
				port = defaultIngressPort
			}
			// Settings only reach a daemon this run starts; one already
			// running keeps the flags it was started with.
			alreadyUp := healthy(qooqdDaemon(nil))
			if port > 0 && !alreadyUp && ingressPortBusy(cfg, port) {
				fmt.Printf("note: port %d is in use by another program, so apps are reached at IP:port this time.\n"+
					"      to use <name>.localhost, pick a free port: qooq config set ingress.port 8080\n", port)
				port = 0
			}
			// The settings first, the caller's --qooqd-arg after: for a flag
			// given twice the last value wins.
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

			// Checked before starting anything: docker being down is the most
			// common reason for this to fail, and it is not something qooq can
			// fix. Failing here with the real reason beats a daemon that
			// starts, cannot reach docker, and exits a second later.
			dockerEndpoint, err := checkDocker()
			if err != nil {
				return err
			}
			var sharedEnv []string
			if dockerEndpoint != "" {
				sharedEnv = append(sharedEnv, "DOCKER_HOST="+dockerEndpoint)
			}

			for _, d := range daemons {
				if err := startDaemon(home, d, sharedEnv); err != nil {
					return err
				}
			}

			if alreadyUp {
				fmt.Println("\nqooq was already running with the settings it started with; changed ones apply after: qooq down && qooq up")
			} else {
				if port > 0 {
					fmt.Printf("\nurls:   %s   (port: qooq config set ingress.port <n>)\n", ingressURLExample(port))
				} else {
					fmt.Println("\nurls:   http://<this machine's IP>:<port>   (names instead: qooq config set ingress.port 80)")
				}
				fmt.Printf("access: %s\n", describeAccess(cfg))
			}
			fmt.Printf("\nstate:  %s\n", home)
			fmt.Printf("logs:   %s\n", filepath.Join(home, "<daemon>.log"))
			// The next things a new user actually does: deploy something, hand
			// it to their AI, and not have to run this again after a reboot.
			fmt.Println("\nnext:   qooq deploy .                     # deploy the project in this folder")
			fmt.Println("        qooq skill install qooq-deploy    # let your AI agent deploy (--agent codex|cursor)")
			fmt.Println("        qooq service install              # start qooq automatically at login")
			fmt.Println("        qooq watch ls                     # what is deployed")
			return nil
		},
	}
	cmd.Flags().BoolVar(&noWatcher, "no-watcher", false, "start only qooqd, without the GitOps watcher")
	cmd.Flags().BoolVar(&ingress, "ingress", false, "give apps <name>.localhost addresses even when `qooq config` has ingress.port 0 (it is on by default)")
	cmd.Flags().BoolVar(&ingressTLS, "ingress-tls", false, "with --ingress, serve HTTPS (see --ingress-tls-issuer for which certificates)")
	cmd.Flags().StringVar(&ingressIssuer, "ingress-tls-issuer", "", `who signs the certificates: "acme" (a public CA — needs a real domain pointing here and reachable 443) or "internal" (Caddy own CA, which is what makes HTTPS work locally, at the cost of trusting its root). Default is qooqd own, currently acme`)
	cmd.Flags().StringArrayVar(&extra, "qooqd-arg", nil, "extra argument to pass through to qooqd (repeatable)")
	return cmd
}

// checkDocker reports whether the container runtime is reachable, by asking
// docker rather than by guessing from the presence of a socket file, and
// returns the endpoint the daemons should use.
//
// The endpoint matters because the docker CLI and the daemons find docker
// differently: the CLI reads *contexts*, while the Go SDK the daemons use
// reads only DOCKER_HOST and otherwise assumes /var/run/docker.sock. OrbStack,
// Colima, Rancher Desktop and podman all set a context and leave DOCKER_HOST
// unset, so `docker info` succeeds here while qooqd fails to connect — this
// check would have declared docker fine and then started a daemon that could
// not reach it.
//
// Empty means "nothing to override": DOCKER_HOST is already set, or the
// context could not be read, in which case the daemons keep their own default.
func checkDocker() (string, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", fmt.Errorf("docker is not installed (or not on your PATH), and qooq needs it to run your apps.\n" +
			"  Install one of these, open it once, then run qooq up again:\n" +
			"    Docker Desktop  https://www.docker.com/products/docker-desktop/\n" +
			"    OrbStack (Mac)  https://orbstack.dev\n" +
			"    Linux           https://docs.docker.com/engine/install/")
	}
	if out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		return "", fmt.Errorf("docker is installed but not responding — start Docker Desktop or OrbStack and try again\n  docker said: %s",
			strings.TrimSpace(firstLine(string(out))))
	}

	// An explicit DOCKER_HOST is the operator's decision and is inherited by
	// the daemons anyway; do not second-guess it.
	if os.Getenv("DOCKER_HOST") != "" {
		return "", nil
	}
	out, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// inheritedTokenEnv keeps the two daemons agreeing on auth. Both inherit the
// shell's environment, and qooqd turns auth on when it finds QOOQ_TOKEN — but
// the watcher reads its qooqd token from QOOQ_WATCHER_QOOQD_TOKEN, so a
// QOOQ_TOKEN left exported in the shell used to give a qooqd that rejected
// every build the watcher sent it ("build failed: missing bearer token").
// An explicit QOOQ_WATCHER_QOOQD_TOKEN still wins.
func inheritedTokenEnv(d daemon, getenv func(string) string) []string {
	if d.name != "qooq-watcher" {
		return nil
	}
	tok := getenv("QOOQ_TOKEN")
	if tok == "" || getenv("QOOQ_WATCHER_QOOQD_TOKEN") != "" {
		return nil
	}
	return []string{"QOOQ_WATCHER_QOOQD_TOKEN=" + tok}
}

// startDaemon launches one daemon and waits for it to answer, unless it is
// already up.
func startDaemon(home string, d daemon, extraEnv []string) error {
	if healthy(d) {
		fmt.Printf("already running  %-14s :%d\n", d.name, d.port)
		return nil
	}
	// Holding the port is not the same as being the daemon. Treating any
	// listener as "already running" would report success and then leave every
	// later command failing against something that is not qooq at all — so
	// the health endpoint, not the port, is what counts as running.
	if listening(d.port) {
		return fmt.Errorf("something is already listening on :%d but it is not answering %s's health endpoint;\n"+
			"stop whatever holds that port, or run %s yourself on another port", d.port, d.name, d.binary)
	}

	bin, err := findDaemonBinary(d.binary)
	if err != nil {
		return err
	}

	logPath := logFile(home, d)
	logOut, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", logPath, err)
	}
	defer logOut.Close()

	c := exec.Command(bin, d.args(home)...)
	c.Dir = home
	c.Stdout = logOut
	c.Stderr = logOut
	c.Env = os.Environ()
	c.Env = append(c.Env, extraEnv...)
	if d.env != nil {
		c.Env = append(c.Env, d.env(home)...)
	}
	c.Env = append(c.Env, inheritedTokenEnv(d, os.Getenv)...)
	detach(c)

	if err := c.Start(); err != nil {
		return fmt.Errorf("start %s: %w", d.name, err)
	}
	if err := os.WriteFile(pidFile(home, d), []byte(fmt.Sprint(c.Process.Pid)), 0o644); err != nil {
		// Not fatal: the daemon is running, and `qooq down` falls back to
		// reporting that it could not identify the process rather than
		// signalling something it cannot name.
		fmt.Fprintf(os.Stderr, "warning: could not write pidfile for %s: %v\n", d.name, err)
	}
	// The parent must not keep the child as a zombie once it exits, and it
	// must not block either; releasing is what makes this a detached start.
	_ = c.Process.Release()

	for i := 0; i < 60; i++ {
		if healthy(d) {
			fmt.Printf("started          %-14s :%d\n", d.name, d.port)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Never report a start that did not happen. The log tail is included
	// because the reason is always in it, and asking someone to go and find
	// the file is a step that helps nobody.
	return fmt.Errorf("%s did not become healthy within 30s\n--- last lines of %s ---\n%s",
		d.name, logPath, tailFile(logPath, 15))
}

func tailFile(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(no log file)"
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func downCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Stop qooqd and qooq-watcher",
		Long: `Stops the daemons started by ` + "`qooq up`" + `.

Deployed components keep running: they are containers owned by docker, not
children of qooqd. Starting qooqd again reconciles with whatever it finds.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := qooqHome()
			if err != nil {
				return err
			}
			// Reverse order: the watcher talks to qooqd, so stopping it first
			// avoids a last sweep failing against a daemon on its way out.
			var problems []string
			for _, d := range []daemon{watcherDaemon(), qooqdDaemon(nil)} {
				if err := stopDaemon(home, d); err != nil {
					problems = append(problems, err.Error())
				}
			}
			if len(problems) > 0 {
				return fmt.Errorf("%s", strings.Join(problems, "\n"))
			}
			return nil
		},
	}
}

// stopDaemon signals a daemon and confirms it actually stopped.
//
// The pid comes from the pidfile, but a pid alone is not trusted: pids are
// reused, and signalling a stale one would kill an unrelated process. The port
// is what confirms a qooq daemon is really there before signalling, and what
// confirms it is really gone afterwards.
func stopDaemon(home string, d daemon) error {
	if !listening(d.port) {
		fmt.Printf("not running      %-14s :%d\n", d.name, d.port)
		_ = os.Remove(pidFile(home, d))
		return nil
	}

	data, err := os.ReadFile(pidFile(home, d))
	if err != nil {
		return fmt.Errorf("%s is listening on :%d but there is no pidfile, so this command cannot tell which process it is; stop it by hand", d.name, d.port)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil || pid <= 0 {
		return fmt.Errorf("%s: pidfile is unreadable; stop it by hand", d.name)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("%s: no process %d; stop it by hand", d.name, pid)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("%s: signalling %d: %w", d.name, pid, err)
	}

	for i := 0; i < 20; i++ {
		if !listening(d.port) {
			fmt.Printf("stopped          %-14s :%d\n", d.name, d.port)
			_ = os.Remove(pidFile(home, d))
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%s still listening on :%d 10s after SIGTERM (pid %d)", d.name, d.port, pid)
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the qooq daemons are running",
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := qooqHome()
			if err != nil {
				return err
			}
			fmt.Printf("state: %s\n\n", home)
			allUp := true
			for _, d := range []daemon{qooqdDaemon(nil), watcherDaemon()} {
				switch {
				case healthy(d):
					fmt.Printf("%-14s :%-5d up\n", d.name, d.port)
				case listening(d.port):
					// Something holds the port but will not answer: either it
					// is still starting, or it is not qooq at all. Both are
					// worth distinguishing from "up".
					allUp = false
					fmt.Printf("%-14s :%-5d listening but not answering health checks\n", d.name, d.port)
				default:
					allUp = false
					fmt.Printf("%-14s :%-5d down\n", d.name, d.port)
				}
			}
			if !allUp {
				fmt.Println("\nstart them with: qooq up")
			}
			return nil
		},
	}
}
