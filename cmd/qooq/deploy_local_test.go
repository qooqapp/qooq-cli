package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qooqapp/qooq/pkg/watcherclient"
)

// fakeUploadWatcher is the slice of qooq-watcher a local deploy talks to: one
// target, created by POST, fed by PUT .../source, read by GET.
type fakeUploadWatcher struct {
	mu        sync.Mutex
	target    *watcherclient.TargetResponse
	created   *watcherclient.CreateTargetRequest
	uploaded  []string // file names in the last archive
	uploads   int
	buildDone bool
}

func (f *fakeUploadWatcher) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/targets":
			var req watcherclient.CreateTargetRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("create body: %v", err)
			}
			f.created = &req
			ns := "default"
			if len(req.DeploySpec) > 0 {
				var spec map[string]string
				_ = json.Unmarshal(req.DeploySpec, &spec)
				ns = spec["namespace"]
			}
			f.target = &watcherclient.TargetResponse{Name: req.Name, DeployMode: req.DeployMode, ComponentName: req.ComponentName, Namespace: ns, LastBuildStatus: "never"}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(f.target)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/targets/"):
			if f.target == nil || r.URL.Path != "/v1/targets/"+f.target.Name {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"target not found"}`))
				return
			}
			if f.buildDone {
				f.target.LastBuildStatus = "success"
				f.target.BuildGeneration = 1
				f.target.ComponentPhase = "Running"
				f.target.Endpoints = []string{"http://192.168.0.5:19001"}
			}
			_ = json.NewEncoder(w).Encode(f.target)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/source"):
			f.uploads++
			f.uploaded = tarNames(t, r.Body)
			f.buildDone = true
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(watcherclient.UploadSourceResponse{Revision: "abc1234def", BuildGeneration: 0, BuildQueued: true})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	})
}

func tarNames(t *testing.T, body io.Reader) []string {
	gz, err := gzip.NewReader(body)
	if err != nil {
		t.Fatalf("upload is not gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatalf("upload is not a tar: %v", err)
		}
		names = append(names, h.Name)
	}
}

func runLocalDeployCmd(t *testing.T, addr, dir string, args ...string) (string, string, error) {
	t.Helper()
	pollEvery = 5 * time.Millisecond
	saved := watcherAddr
	t.Cleanup(func() { watcherAddr = saved })
	cmd := deployDirCmd()
	var out, errs bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errs)
	cmd.SetArgs(append([]string{dir, "--watcher-addr", addr}, args...))
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errs.String(), err
}

func TestLocalDeployCreatesAnUploadTargetAndPrintsTheURL(t *testing.T) {
	f := &fakeUploadWatcher{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.html": "<h1>hi</h1>", ".env": "SECRET=1"})

	out, errs, err := runLocalDeployCmd(t, srv.URL, dir, "--name", "site", "--timeout", "2s")
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, errs)
	}
	if f.created == nil || f.created.DeployMode != "upload" || f.created.ComponentName != "site" || f.created.Name != "site" {
		t.Fatalf("created %+v, want an upload target named site", f.created)
	}
	if !f.created.PublishDetectedPorts {
		t.Fatalf("a local deploy must publish the detected port, or it comes up with no URL")
	}
	if len(f.created.DeploySpec) != 0 {
		t.Fatalf("default namespace should send no deploy_spec, got %s", f.created.DeploySpec)
	}
	joined := strings.Join(f.uploaded, ",")
	if !strings.Contains(joined, "index.html") || strings.Contains(joined, ".env") {
		t.Fatalf("uploaded %v: want index.html and never .env", f.uploaded)
	}
	if !strings.Contains(errs, "deployed: http://192.168.0.5:19001") || !strings.Contains(out, "http://192.168.0.5:19001") {
		t.Fatalf("URL not reported:\nstdout: %s\nstderr: %s", out, errs)
	}
	if !strings.Contains(errs, "not uploading 1 env file") {
		t.Fatalf("the skipped .env is not mentioned: %s", errs)
	}
}

func TestLocalDeployRefusesAGitTargetBeforeUploading(t *testing.T) {
	f := &fakeUploadWatcher{target: &watcherclient.TargetResponse{Name: "site", DeployMode: "source", RepoURL: "file:///repo.git"}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.html": "x"})

	_, _, err := runLocalDeployCmd(t, srv.URL, dir, "--name", "site")
	if err == nil || !strings.Contains(err.Error(), "file:///repo.git") || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("err = %v, want a refusal naming the repository and --name", err)
	}
	if f.uploads != 0 {
		t.Fatalf("uploaded %d times to a git target", f.uploads)
	}
}

func TestLocalDeploySaysToRunQooqUpWhenNothingListens(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "http://" + ln.Addr().String()
	ln.Close() // a port nothing answers on

	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.html": "x"})
	_, _, err = runLocalDeployCmd(t, addr, dir, "--name", "site")
	if err == nil || !strings.Contains(err.Error(), "qooq up") {
		t.Fatalf("err = %v, want it to say `qooq up`", err)
	}
}

func TestLocalDeployInANamespace(t *testing.T) {
	f := &fakeUploadWatcher{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.html": "x"})

	if _, errs, err := runLocalDeployCmd(t, srv.URL, dir, "--name", "api", "-n", "shop", "--timeout", "2s"); err != nil {
		t.Fatalf("deploy: %v\n%s", err, errs)
	}
	if f.created == nil || f.created.Name != "shop_api" || f.created.ComponentName != "api" {
		t.Fatalf("created %+v, want target shop_api for component api", f.created)
	}
	if !strings.Contains(string(f.created.DeploySpec), `"namespace":"shop"`) {
		t.Fatalf("deploy_spec %s does not carry the namespace", f.created.DeploySpec)
	}
}
