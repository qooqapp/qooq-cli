package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

// composeCmd manages the whole qooqd + qooq-watcher compose stack as one unit.
// It shells out to `docker compose` with the compose files found in --dir —
// the CLI stays a thin wrapper and never imports the docker SDK (dependency
// wall, see make check).
// firstEnv returns the first of these environment variables that is set.
// QOOQ_STACK_DIR is the pre-rename name of QOOQ_COMPOSE_DIR, still honored so
// an existing shell profile keeps working: `qooq stack` now means something
// else entirely (a group of deployed projects), but the directory it used to
// point at has not moved.
func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func composeCmd() *cobra.Command {
	var dir string

	cmd := &cobra.Command{
		Use:   "compose",
		Short: "Start/stop the whole qooqd + qooq-watcher compose stack",
		Long: `Wraps docker compose over the compose files in --dir:
docker-compose.yml, plus docker-compose.e2e.yml when present (the local
GitOps demo overlay). Compose env (QOOQ_TOKEN, QOOQ_WATCHER_SECRET_KEY, ...)
comes from the caller's environment or a .env file in --dir.`,
	}

	defDir := firstEnv("QOOQ_COMPOSE_DIR", "QOOQ_STACK_DIR")
	if defDir == "" {
		defDir = "."
	}
	cmd.PersistentFlags().StringVar(&dir, "dir", defDir,
		"directory containing docker-compose.yml (default: QOOQ_COMPOSE_DIR env, else current dir)")

	var build bool
	up := &cobra.Command{
		Use:   "up",
		Short: "Start the stack and wait until healthy (docker compose up -d --wait)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			args := []string{"up", "-d", "--wait"}
			if build {
				args = append(args, "--build")
			}
			return runCompose(cmd, dir, args...)
		},
	}
	up.Flags().BoolVar(&build, "build", false, "rebuild images before starting")

	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop the stack, keeping containers and data (docker compose stop)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCompose(cmd, dir, "stop")
		},
	}

	var volumes bool
	down := &cobra.Command{
		Use:   "down",
		Short: "Remove the stack's containers (docker compose down)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			args := []string{"down"}
			if volumes {
				args = append(args, "-v")
			}
			return runCompose(cmd, dir, args...)
		},
	}
	down.Flags().BoolVarP(&volumes, "volumes", "v", false,
		"ALSO DELETE the data volumes (components/targets state) — irreversible")

	status := &cobra.Command{
		Use:   "status",
		Short: "Show the stack's services and health (docker compose ps)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCompose(cmd, dir, "ps")
		},
	}

	cmd.AddCommand(up, stop, down, status)
	return cmd
}

// runCompose execs `docker compose -f ... <extra>` with cwd set to dir, so
// compose picks up dir/.env and relative paths in the compose files (e.g. the
// e2e overlay's ../qooq-demo.git bind mount) resolve as if run from there.
func runCompose(cmd *cobra.Command, dir string, extra ...string) error {
	if err := ensureDockerAvailable(cmd); err != nil {
		return err
	}
	files, err := composeFiles(dir)
	if err != nil {
		return err
	}
	args := []string{"compose"}
	for _, f := range files {
		args = append(args, "-f", f)
	}
	args = append(args, extra...)
	c := exec.CommandContext(cmd.Context(), "docker", args...)
	c.Dir = dir
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// ensureDockerAvailable checks the two ways a customer machine can be missing
// the `stack` command's only external dependency: no Docker Engine at all, or
// an engine without the Compose v2 plugin (older installs only ship the
// classic docker-compose binary, which this CLI does not shell out to).
func ensureDockerAvailable(cmd *cobra.Command) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker not found in PATH — install Docker Engine (https://docs.docker.com/engine/install/) before running qooq compose commands")
	}
	if err := exec.CommandContext(cmd.Context(), "docker", "compose", "version").Run(); err != nil {
		return fmt.Errorf("docker compose plugin not available — install the Compose v2 plugin (https://docs.docker.com/compose/install/) before running qooq compose commands")
	}
	return nil
}

func composeFiles(dir string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err != nil {
		return nil, fmt.Errorf("no docker-compose.yml in %q — point --dir (or QOOQ_COMPOSE_DIR) at the qooq repo", dir)
	}
	files := []string{"docker-compose.yml"}
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.e2e.yml")); err == nil {
		files = append(files, "docker-compose.e2e.yml")
	}
	return files, nil
}
