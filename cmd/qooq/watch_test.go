package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/qooqapp/qooq/pkg/watcherclient"
)

// The lifecycle verbs are thin, which is exactly why they are worth pinning:
// each one is a route and a sentence, and the ways they go wrong are a verb
// wired to its neighbour's route (stop calling pause), `rm` quietly going back
// to leaving the container running, or a success line printed for something
// the watcher refused. None of those fail to compile, and smoke only notices
// the first after four minutes of real docker.

type recordedRequest struct {
	method, path, auth string
}

// fakeWatcher answers every request with status and records what it was asked.
func fakeWatcher(t *testing.T, status int) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []recordedRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, recordedRequest{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization")})
		mu.Unlock()
		if status >= 400 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"target \"web\" not found"}`)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), reqs...)
	}
}

// runWatch runs `qooq watch <args>` against addr and returns what it printed.
func runWatch(t *testing.T, addr string, args ...string) (string, error) {
	t.Helper()

	// fmt.Printf goes straight to os.Stdout, so that is what has to be caught.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	cmd := watchCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(append([]string{"--watcher-addr", addr, "--watcher-token", "tok"}, args...))
	runErr := cmd.ExecuteContext(context.Background())

	_ = w.Close()
	return <-done, runErr
}

func TestLifecycleVerbsCallTheirOwnRoutes(t *testing.T) {
	cases := []struct {
		args       []string
		method     string
		path       string
		wantOutput string
	}{
		{[]string{"stop", "web"}, http.MethodPost, "/v1/targets/web/stop", "stopped web"},
		{[]string{"start", "web"}, http.MethodPost, "/v1/targets/web/start", "started web"},
		{[]string{"pause", "web"}, http.MethodPost, "/v1/targets/web/pause", "paused automatic deploys for web"},
		{[]string{"resume", "web"}, http.MethodPost, "/v1/targets/web/resume", "resumed automatic deploys for web"},
		{[]string{"trigger", "web"}, http.MethodPost, "/v1/targets/web/trigger", "triggered web"},
		{[]string{"trigger", "--force", "web"}, http.MethodPost, "/v1/targets/web/trigger?force=1", "triggered web"},
		// The default is the one that changed: rm takes the deployment with it.
		// A request carrying keep_component here would be the old behaviour
		// back, with nothing on screen to say so.
		{[]string{"rm", "web"}, http.MethodDelete, "/v1/targets/web", "removed target web and its deployment"},
		{[]string{"rm", "--keep-component", "web"}, http.MethodDelete, "/v1/targets/web?keep_component=1", "its deployment was left in place"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			srv, requests := fakeWatcher(t, http.StatusNoContent)

			out, err := runWatch(t, srv.URL, tc.args...)
			if err != nil {
				t.Fatalf("command failed: %v", err)
			}

			got := requests()
			if len(got) != 1 {
				t.Fatalf("want exactly one request, got %d: %+v", len(got), got)
			}
			if got[0].method != tc.method || got[0].path != tc.path {
				t.Errorf("sent %s %s, want %s %s", got[0].method, got[0].path, tc.method, tc.path)
			}
			if got[0].auth != "Bearer tok" {
				t.Errorf("Authorization = %q, want the --watcher-token", got[0].auth)
			}
			if !strings.Contains(out, tc.wantOutput) {
				t.Errorf("printed %q, want it to contain %q", out, tc.wantOutput)
			}
		})
	}
}

// A refused request must come back as an error and must not print the line
// that says it worked — "stopped web" on a 404 is the CLI lying.
func TestLifecycleVerbsDoNotClaimSuccessWhenRefused(t *testing.T) {
	for _, verb := range []string{"stop", "start", "pause", "resume", "rm", "trigger"} {
		t.Run(verb, func(t *testing.T) {
			srv, _ := fakeWatcher(t, http.StatusNotFound)

			out, err := runWatch(t, srv.URL, verb, "web")
			if err == nil {
				t.Fatal("want an error for a 404, got none")
			}
			if !watcherclient.IsNotFound(err) {
				t.Errorf("error lost its status: %v", err)
			}
			if strings.TrimSpace(out) != "" {
				t.Errorf("printed %q for a refused request", out)
			}
		})
	}
}

func TestLifecycleVerbsNeedExactlyOneName(t *testing.T) {
	for _, verb := range []string{"stop", "start", "pause", "resume", "rm", "trigger"} {
		for _, args := range [][]string{{verb}, {verb, "a", "b"}} {
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				srv, requests := fakeWatcher(t, http.StatusNoContent)
				if _, err := runWatch(t, srv.URL, args...); err == nil {
					t.Error("want an argument error")
				}
				if n := len(requests()); n != 0 {
					t.Errorf("sent %d requests before rejecting the arguments", n)
				}
			})
		}
	}
}

// buildStatus is how `watch ls` tells apart the two ways a target is not
// deploying. The watcher's own give-up wins over the operator's pause: it is
// the one that needs somebody to act.
func TestBuildStatusNamesBothKindsOfPause(t *testing.T) {
	cases := []struct {
		name string
		t    watcherclient.TargetResponse
		want string
	}{
		{"plain", watcherclient.TargetResponse{LastBuildStatus: "success"}, "success"},
		{"paused", watcherclient.TargetResponse{LastBuildStatus: "success", DeployPaused: true}, "success (paused)"},
		{"gave up", watcherclient.TargetResponse{LastBuildStatus: "failed", PausedReason: "10 failures"}, "failed (gave up)"},
		{"both", watcherclient.TargetResponse{LastBuildStatus: "failed", DeployPaused: true, PausedReason: "10 failures"}, "failed (gave up)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildStatus(tc.t); got != tc.want {
				t.Errorf("buildStatus = %q, want %q", got, tc.want)
			}
		})
	}
}
