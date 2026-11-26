package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qooqapp/qooq/pkg/client"
)

// `qooq secret`: the namespace secret store that ${secret.NAME} in qooq.yaml
// reads from. The watcher has had the API for a long time; without a command
// the only way in was curl, which is no way at all for most people deploying.

// secretNamePattern is what a name must look like: an environment variable's
// shape, because that is what it usually becomes and what ${secret.NAME}
// reads cleanly.
var secretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func checkSecretName(name string) error {
	if !secretNamePattern.MatchString(name) {
		return fmt.Errorf("%q is not a valid secret name: use letters, digits and underscores, not starting with a digit (like STRIPE_KEY)", name)
	}
	return nil
}

func secretCmd() *cobra.Command {
	var ns string
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage secrets that qooq.yaml reads as ${secret.NAME}",
		Long: `Manage the secrets a namespace's apps read as ${secret.NAME} in qooq.yaml.

Values are stored by qooq-watcher and never printed again; ` + "`ls`" + ` shows names only.
A deploy picks up the current values, so redeploy after changing one.`,
	}
	cmd.PersistentFlags().StringVarP(&ns, "namespace", "n", "default", "namespace the secrets belong to")
	cmd.PersistentFlags().StringVar(&watcherAddr, "watcher-addr", watcherAddr, "qooq-watcher base URL (default: QOOQ_WATCHER_ADDR env)")
	ns2 := func() string { return client.NormalizeNamespace(ns) }

	set := &cobra.Command{
		Use:   "set NAME=VALUE [NAME=VALUE ...]",
		Short: "Store secrets (NAME=@file reads the value from a file, keeping it out of shell history)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			values := map[string]string{}
			for _, pair := range args {
				name, raw, found := strings.Cut(pair, "=")
				if !found {
					return fmt.Errorf("%q: expected NAME=VALUE (or NAME=@file)", pair)
				}
				if err := checkSecretName(name); err != nil {
					return err
				}
				v, err := atFileValue(raw)
				if err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				values[name] = v
			}
			return putSecrets(cmd, ns2(), values)
		},
	}

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List the names of a namespace's secrets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			names, err := newWatcherClient().ListSecrets(cmd.Context(), ns2())
			if err != nil {
				return explainLocal(err)
			}
			if len(names) == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "no secrets in namespace %s\n", ns2())
				return nil
			}
			sort.Strings(names)
			for _, n := range names {
				fmt.Fprintln(cmd.OutOrStdout(), n)
			}
			return nil
		},
	}

	rm := &cobra.Command{
		Use:   "rm NAME [NAME ...]",
		Short: "Delete secrets",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wc := newWatcherClient()
			for _, name := range args {
				if err := wc.DeleteSecret(cmd.Context(), ns2(), name); err != nil {
					return fmt.Errorf("%s: %w", name, explainLocal(err))
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "deleted %s\n", name)
			}
			return nil
		},
	}

	imp := &cobra.Command{
		Use:   "import [FILE]",
		Short: "Store every KEY=VALUE in a .env file (default ./.env) as a secret",
		Long: `Store every KEY=VALUE in a .env file (default ./.env) as a secret, so the keys an
app already has locally do not have to be typed again. Comments, blank lines and an
"export " prefix are skipped; surrounding quotes are removed. Reference them in
qooq.yaml as ${secret.KEY}.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := ".env"
			if len(args) == 1 {
				path = args[0]
			}
			values, err := parseDotenv(path)
			if err != nil {
				return err
			}
			if len(values) == 0 {
				return fmt.Errorf("%s has no KEY=VALUE lines", path)
			}
			return putSecrets(cmd, ns2(), values)
		},
	}

	cmd.AddCommand(set, ls, rm, imp)
	return cmd
}

func putSecrets(cmd *cobra.Command, ns string, values map[string]string) error {
	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	sort.Strings(names)
	wc := newWatcherClient()
	for _, n := range names {
		if err := wc.PutSecret(cmd.Context(), ns, n, values[n]); err != nil {
			return fmt.Errorf("%s: %w", n, explainLocal(err))
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "stored %s\n", n)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "use them in qooq.yaml as ${secret.%s}; redeploy to pick up changed values\n", names[0])
	return nil
}

// parseDotenv reads the common .env shape: KEY=VALUE per line, # comments,
// an optional "export " prefix, values optionally in matching quotes. Not a
// full shell parser — no interpolation, no multi-line values — which is also
// what most .env loaders do.
func parseDotenv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		key, val, found := strings.Cut(s, "=")
		if !found {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, line)
		}
		key = strings.TrimSpace(key)
		if err := checkSecretName(key); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		out[key] = val
	}
	return out, sc.Err()
}
