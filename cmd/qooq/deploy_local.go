package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qooqapp/qooq/pkg/client"
	"github.com/qooqapp/qooq/pkg/watcherclient"
)

// The local half of `qooq deploy`: the same packing as the cloud path, sent
// to this machine's qooq-watcher as an upload target (deploy_mode "upload",
// PUT /v1/targets/{name}/source). No git, no account — the case of somebody
// who just wants the directory in front of them running.

// localTargetName is the watcher target an upload of name/ns lives under:
// the bare name in the default namespace (what `qooq watch ls` shows for a
// project deployed the git way, too), "<ns>_<name>" elsewhere, because target
// names are global and two namespaces may both have an "api".
func localTargetName(name, ns string) string {
	if client.NormalizeNamespace(ns) == client.DefaultNamespace {
		return name
	}
	return ns + "_" + name
}

func runLocalDeploy(cmd *cobra.Command, dir, flagName, ns string, force, noWait bool, timeout time.Duration, output string) error {
	errw := cmd.ErrOrStderr()
	if output != "table" && output != "json" {
		return fmt.Errorf("--output must be table or json, got %q", output)
	}
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
	ns = client.NormalizeNamespace(ns)
	tn := localTargetName(name, ns)
	wc := newWatcherClient()

	t, err := wc.GetTarget(cmd.Context(), tn)
	switch {
	case err == nil:
		if t.DeployMode != "upload" {
			src := t.RepoURL
			if src == "" {
				src = t.ImageRef
			}
			return fmt.Errorf("%s already exists and deploys from %s (%s mode), so a directory cannot be uploaded to it: push to that repository instead, or pick another name with --name", tn, orDash(src), t.DeployMode)
		}
	case watcherclient.StatusCode(err) == http.StatusNotFound:
		t = nil
	default:
		return explainLocal(err)
	}

	set, err := listFiles(dir)
	if err != nil {
		return err
	}
	if len(set.EnvSkipped) > 0 {
		fmt.Fprintf(errw, "not uploading %d env file(s): %s — store them with `qooq secret import` and reference ${secret.NAME} in qooq.yaml (or declare inputs: and use `qooq watch set-input %s`)\n",
			len(set.EnvSkipped), strings.Join(set.EnvSkipped, ", "), tn)
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

	// Created only once the archive is known to be acceptable: a target that
	// exists but never got a source would sit in `watch ls` as "never" for no
	// reason anyone could see.
	if t == nil {
		// PublishDetectedPorts: with no qooq.yaml the watcher guesses the
		// port, and normally keeps a guessed port off the host — publishing is
		// the operator's call for a repo it merely picked up. Here the operator
		// is the person who just ran `qooq deploy`, and a deploy that answers
		// "running, no URL" is not what they asked for.
		req := watcherclient.CreateTargetRequest{Name: tn, DeployMode: "upload", ComponentName: name, PublishDetectedPorts: true}
		if ns != client.DefaultNamespace {
			spec, err := json.Marshal(map[string]string{"namespace": ns})
			if err != nil {
				return err
			}
			req.DeploySpec = spec
		}
		if t, err = wc.CreateTarget(cmd.Context(), req); err != nil {
			return explainLocal(err)
		}
		fmt.Fprintf(errw, "created %s\n", tn)
	}

	f, err := os.Open(pk.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(errw, "uploading to %s ...\n", tn)
	res, err := wc.UploadSource(cmd.Context(), tn, f, force)
	if err != nil {
		return explainLocal(err)
	}

	if !res.BuildQueued {
		cur, err := wc.GetTarget(cmd.Context(), tn)
		if err != nil {
			return explainLocal(err)
		}
		fmt.Fprintln(errw, "the content has not changed, so nothing was redeployed (use --force to rebuild anyway)")
		if len(cur.Endpoints) > 0 {
			fmt.Fprintln(errw, "running at: "+strings.Join(cur.Endpoints, ", "))
		}
		return printLocal(cmd.OutOrStdout(), output, cur)
	}
	fmt.Fprintf(errw, "uploaded revision %s\n", shortCommit(res.Revision))
	if noWait {
		if output == "json" {
			return writeJSON(cmd.OutOrStdout(), res)
		}
		return nil
	}
	return waitLocal(cmd, wc, tn, res.BuildGeneration, t.LastBuildAt, timeout, output)
}

// waitLocal polls until a build newer than the upload's baseline finishes,
// then reports. Same rule as the cloud path (newBuildDone): a status alone
// proves nothing, because until the sweep picks the upload up the target
// still describes the previous build in every field.
func waitLocal(cmd *cobra.Command, wc *watcherclient.Client, tn string, baseGen uint64, baseAt *time.Time, timeout time.Duration, output string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()
	errw := cmd.ErrOrStderr()
	last := ""
	for {
		cur, err := wc.GetTarget(ctx, tn)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return explainLocal(err)
		}
		if cur.LastBuildStatus != last {
			last = cur.LastBuildStatus
			fmt.Fprintf(errw, "status: %s\n", last)
		}
		if localBuildDone(baseGen, baseAt, cur) {
			return finishLocal(cmd, wc, cur, output)
		}
		select {
		case <-ctx.Done():
		case <-time.After(pollEvery):
			continue
		}
		break
	}
	return fmt.Errorf("timed out after %s waiting for %s: no finished new build observed (last seen status %q) — see `qooq watch get %s`", timeout, tn, last, tn)
}

func localBuildDone(baseGen uint64, baseAt *time.Time, cur *watcherclient.TargetResponse) bool {
	switch cur.LastBuildStatus {
	case statusBuilding, statusNever, "":
		return false
	}
	if cur.BuildGeneration > baseGen {
		return true
	}
	return cur.LastBuildAt != nil && (baseAt == nil || cur.LastBuildAt.After(*baseAt))
}

func finishLocal(cmd *cobra.Command, wc *watcherclient.Client, t *watcherclient.TargetResponse, output string) error {
	out, errw := cmd.OutOrStdout(), cmd.ErrOrStderr()
	_ = printLocal(out, output, t)
	switch t.LastBuildStatus {
	case statusSuccess:
		if len(t.Endpoints) == 0 {
			fmt.Fprintln(errw, "deployed: no URL (the app publishes no port)")
		} else {
			fmt.Fprintln(errw, "deployed: "+strings.Join(t.Endpoints, ", "))
		}
		if t.ComponentPhase == "Failed" || t.ComponentPhase == "Exited" {
			return fmt.Errorf("the build succeeded but the app is %s%s — its output: `qooq watch logs %s`", t.ComponentPhase, suffix(": ", t.ComponentError), t.Name)
		}
		return nil
	case statusNeedsInput:
		key := "NAME"
		if len(t.MissingInputs) > 0 {
			key = t.MissingInputs[0]
		}
		return fmt.Errorf("needs input: qooq.yaml declares values that have no answer yet (%s). Supply them with `qooq watch set-input %s %s=<value>`; that redeploys",
			strings.Join(t.MissingInputs, ", "), t.Name, key)
	default:
		msg, _, _ := strings.Cut(orDash(t.LastBuildError), "\n")
		fmt.Fprintln(errw, "deploy "+t.LastBuildStatus+": "+msg)
		if text, err := wc.BuildLog(cmd.Context(), t.Name); err == nil && strings.TrimSpace(text) != "" {
			fmt.Fprintf(errw, "--- last lines of the build output ---\n%s\n", tailLines(text, 30))
		}
		return fmt.Errorf("deploy of %s %s — full build output: `qooq watch build-log %s`", t.Name, t.LastBuildStatus, t.Name)
	}
}

func printLocal(w io.Writer, output string, t *watcherclient.TargetResponse) error {
	if output == "json" {
		return writeJSON(w, t)
	}
	fmt.Fprintf(w, "name:     %s\n", t.ComponentName)
	fmt.Fprintf(w, "status:   %s\n", orDash(t.LastBuildStatus))
	if t.ComponentPhase != "" {
		fmt.Fprintf(w, "running:  %s\n", t.ComponentPhase)
	}
	if len(t.Endpoints) > 0 {
		fmt.Fprintf(w, "url:      %s\n", strings.Join(t.Endpoints, ", "))
	}
	return nil
}

// explainLocal turns "nothing is listening" into the one command that fixes
// it; other errors pass through.
func explainLocal(err error) error {
	if watcherclient.StatusCode(err) == 0 && strings.Contains(err.Error(), "connection refused") {
		return fmt.Errorf("qooq is not running on this machine (nothing answered at %s): start it with `qooq up`, or deploy to the cloud with `qooq deploy --cloud`", watcherAddr)
	}
	if watcherclient.StatusCode(err) == http.StatusUnauthorized {
		return fmt.Errorf("qooq-watcher at %s wants a token: set QOOQ_WATCHER_TOKEN", watcherAddr)
	}
	return err
}
