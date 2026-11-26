package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// localConfig is what `qooq up` (and `qooq service install`) start the daemons
// with, kept in ~/.qooq/config.yaml so a choice made once survives restarts
// instead of having to be repeated as flags every time.
type localConfig struct {
	// IngressPort is where apps get their <name>.localhost address. nil means
	// the default (80); 0 turns the ingress off and apps are reached by
	// IP:port only.
	IngressPort *int `yaml:"ingress_port,omitempty"`
	// Access is who can open the apps: "lan" (this machine and the local
	// network — a phone on the same Wi-Fi) or "local" (this machine only).
	Access string `yaml:"access,omitempty"`
}

const (
	defaultIngressPort = 80
	accessLAN          = "lan"
	accessLocal        = "local"
)

func (c localConfig) ingressPort() int {
	if c.IngressPort == nil {
		return defaultIngressPort
	}
	return *c.IngressPort
}

func (c localConfig) access() string {
	if c.Access == "" {
		return accessLAN
	}
	return c.Access
}

func configPath(home string) string { return filepath.Join(home, "config.yaml") }

func loadLocalConfig(home string) (localConfig, error) {
	var c localConfig
	data, err := os.ReadFile(configPath(home))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("%s: %w", configPath(home), err)
	}
	return c, c.validate()
}

func (c localConfig) validate() error {
	if p := c.ingressPort(); p < 0 || p > 65535 {
		return fmt.Errorf("ingress.port %d is not a port (0 turns the ingress off)", p)
	}
	if a := c.access(); a != accessLAN && a != accessLocal {
		return fmt.Errorf("access %q: use %q (this machine and the local network) or %q (this machine only)", a, accessLAN, accessLocal)
	}
	return nil
}

func saveLocalConfig(home string, c localConfig) error {
	if err := c.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(home), data, 0o644)
}

// qooqdArgs turns the settings into qooqd flags. One place for both `up` and
// `service install`: two translations of the same file would drift, and the
// one nobody runs that week would be the wrong one.
func (c localConfig) qooqdArgs(ingressPort int) []string {
	var args []string
	if ingressPort > 0 {
		args = append(args, "-ingress", "-ingress-http-port", strconv.Itoa(ingressPort))
	}
	if c.access() == accessLocal {
		// Both: -publish-ip covers every app's own port, and the ingress
		// names its bind address explicitly (it is the one component a server
		// wants reachable from outside), so it needs its own.
		args = append(args, "-publish-ip", "127.0.0.1", "-ingress-bind-ip", "127.0.0.1")
	}
	return args
}

// ingressPortBusy reports whether something else already holds the port the
// ingress would publish. Only "in use" counts: on Linux an ordinary user may
// not bind below 1024, but docker publishes the port, not this process.
func ingressPortBusy(c localConfig, port int) bool {
	host := "0.0.0.0"
	if c.access() == accessLocal {
		host = "127.0.0.1"
	}
	ln, err := net.Listen("tcp4", net.JoinHostPort(host, strconv.Itoa(port)))
	if err == nil {
		ln.Close()
		return false
	}
	return errors.Is(err, syscall.EADDRINUSE)
}

func describeAccess(c localConfig) string {
	if c.access() == accessLocal {
		return "local — only this machine can open your apps (other devices too: qooq config set access lan)"
	}
	return "lan — this machine and devices on the same network can open your apps (this machine only: qooq config set access local)"
}

func configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or change how `qooq up` runs qooq on this machine",
		Long: `Show or change the settings ` + "`qooq up`" + ` and ` + "`qooq service install`" + ` start qooq with,
kept in ~/.qooq/config.yaml.

  ingress.port   port apps get a <name>.localhost address on (default 80; 0 = off)
  access         lan (default): this machine and the local network can open apps
                 local: this machine only

Changes take effect the next time qooq starts: qooq down && qooq up.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := qooqHome()
			if err != nil {
				return err
			}
			c, err := loadLocalConfig(home)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			port := c.ingressPort()
			if port == 0 {
				fmt.Fprintln(out, "ingress.port  0 (off: apps are reached at IP:port)")
			} else {
				fmt.Fprintf(out, "ingress.port  %d (apps at %s)\n", port, ingressURLExample(port))
			}
			fmt.Fprintf(out, "access        %s\n", describeAccess(c))
			return nil
		},
	}
	set := &cobra.Command{
		Use:   "set KEY VALUE",
		Short: "Change a setting (ingress.port, access)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := qooqHome()
			if err != nil {
				return err
			}
			c, err := loadLocalConfig(home)
			if err != nil {
				return err
			}
			switch args[0] {
			case "ingress.port":
				p, err := strconv.Atoi(args[1])
				if err != nil {
					return fmt.Errorf("ingress.port must be a number (0 turns the ingress off)")
				}
				c.IngressPort = &p
			case "access":
				c.Access = args[1]
			default:
				return fmt.Errorf("unknown setting %q: use ingress.port or access", args[0])
			}
			if err := saveLocalConfig(home, c); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "saved; takes effect the next time qooq starts (qooq down && qooq up)\n")
			return nil
		},
	}
	unset := &cobra.Command{
		Use:   "unset KEY",
		Short: "Return a setting to its default",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := qooqHome()
			if err != nil {
				return err
			}
			c, err := loadLocalConfig(home)
			if err != nil {
				return err
			}
			switch args[0] {
			case "ingress.port":
				c.IngressPort = nil
			case "access":
				c.Access = ""
			default:
				return fmt.Errorf("unknown setting %q: use ingress.port or access", args[0])
			}
			return saveLocalConfig(home, c)
		},
	}
	cmd.AddCommand(set, unset)
	return cmd
}

func ingressURLExample(port int) string {
	if port == 80 {
		return "http://<name>.localhost"
	}
	return fmt.Sprintf("http://<name>.localhost:%d", port)
}
