package main

import (
	"bytes"
	"context"
	"fmt"
	"github.com/qooqapp/qooq/pkg/controlclient"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeControl serves a scripted sequence of project lists: each GET of the
// list returns the next entry (the last one repeats).
type fakeControl struct {
	mu       sync.Mutex
	lists    []string
	n        int
	posts    []string
	buildLog string
	// logQueries records the query of each app-logs request.
	logQueries []string
}

func (f *fakeControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"code":"unauthorized","message":"no"}}`))
		return
	}
	switch {
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/projects"):
		i := f.n
		if i >= len(f.lists) {
			i = len(f.lists) - 1
		}
		f.n++
		w.Write([]byte(f.lists[i]))
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/projects"):
		f.posts = append(f.posts, r.URL.Path)
		w.WriteHeader(201)
		w.Write([]byte(`{"name":"web","namespace":"default","last_build_status":"never"}`))
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/deploy"):
		f.posts = append(f.posts, r.URL.Path)
		w.WriteHeader(202)
		w.Write([]byte(`{"ok":true}`))
	case strings.HasSuffix(r.URL.Path, "/build-log"):
		w.Write([]byte(f.buildLog))
	case strings.HasSuffix(r.URL.Path, "/logs"):
		f.logQueries = append(f.logQueries, r.URL.RawQuery)
		w.Write([]byte("app says hello\n"))
	default:
		w.WriteHeader(404)
	}
}

func list(p string) string { return `{"projects":[` + p + `]}` }

func proj(status string, gen int, at string, extra string) string {
	s := fmt.Sprintf(`{"name":"web","namespace":"default","source":"git","last_build_status":%q,"build_generation":%d`, status, gen)
	if at != "" {
		s += fmt.Sprintf(`,"last_build_at":%q`, at)
	}
	return s + extra + "}"
}

func runProject(t *testing.T, srv *httptest.Server, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	pollEvery = 5 * time.Millisecond
	cmd := projectCmd()
	var o, e bytes.Buffer
	cmd.SetOut(&o)
	cmd.SetErr(&e)
	cmd.SetArgs(append(args, "--addr", srv.URL))
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err = cmd.ExecuteContext(context.Background())
	return o.String(), e.String(), err
}

func signIn(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("QOOQ_HOME", t.TempDir())
	if err := saveLogin(srv.URL, storedLogin{Token: "tok", Account: "acme"}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateWaitSuccess(t *testing.T) {
	f := &fakeControl{lists: []string{
		list(proj("never", 0, "", "")),
		list(proj("building", 1, "", "")),
		list(proj("success", 1, "2026-01-01T00:00:10Z", `,"endpoints":["http://h:19001"],"last_build_revision":"abcdef1234567"`)),
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	signIn(t, srv)
	out, errOut, err := runProject(t, srv, "create", "web", "--repo", "https://g/x", "--wait", "-o", "json")
	if err != nil {
		t.Fatalf("err=%v stderr=%s", err, errOut)
	}
	if !strings.Contains(out, `"http://h:19001"`) || !strings.Contains(errOut, "deployed: http://h:19001") {
		t.Fatalf("stdout=%s stderr=%s", out, errOut)
	}
}

func TestWaitFailureShowsErrorAndLog(t *testing.T) {
	f := &fakeControl{
		lists: []string{
			list(proj("never", 0, "", "")),
			list(proj("failed", 1, "2026-01-01T00:00:10Z", `,"last_build_error":"boom happened"`)),
		},
		buildLog: "step1\nCOMPILE ERROR x.go:3\n",
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	signIn(t, srv)
	_, errOut, err := runProject(t, srv, "create", "web", "--repo", "https://g/x", "--wait")
	if err == nil {
		t.Fatal("want non-zero exit on failed build")
	}
	if !strings.Contains(errOut, "boom happened") || !strings.Contains(errOut, "COMPILE ERROR x.go:3") {
		t.Fatalf("stderr=%s", errOut)
	}
}

func TestWaitIgnoresStalePreviousSuccess(t *testing.T) {
	old := proj("success", 4, "2026-01-01T00:00:00Z", "")
	f := &fakeControl{lists: []string{
		list(old), // baseline read by deploy
		list(old), // trigger queued, worker has not picked it up: stale success
		list(old),
		list(proj("building", 5, "2026-01-01T00:00:00Z", "")),
		list(proj("failed", 5, "2026-01-01T00:05:00Z", `,"last_build_error":"new build broke"`)),
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	signIn(t, srv)
	_, errOut, err := runProject(t, srv, "deploy", "web", "--wait")
	// Accepting the stale success would return nil here.
	if err == nil || !strings.Contains(errOut, "new build broke") {
		t.Fatalf("err=%v stderr=%s", err, errOut)
	}
	if f.n < 5 {
		t.Fatalf("returned after %d lists, before the new build finished", f.n)
	}
}

func TestWaitTimesOutWhenNoNewBuildAppears(t *testing.T) {
	old := proj("success", 4, "2026-01-01T00:00:00Z", "")
	f := &fakeControl{lists: []string{list(old)}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	signIn(t, srv)
	_, _, err := runProject(t, srv, "deploy", "web", "--wait", "--timeout", "100ms")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err=%v", err)
	}
}

func TestNotSignedIn(t *testing.T) {
	srv := httptest.NewServer(&fakeControl{lists: []string{list("")}})
	defer srv.Close()
	t.Setenv("QOOQ_HOME", t.TempDir())
	_, _, err := runProject(t, srv, "ls")
	if err == nil || !strings.Contains(err.Error(), "qooq auth login") {
		t.Fatalf("err=%v", err)
	}
}

func TestExpiredTokenSaysLogInAgain(t *testing.T) {
	srv := httptest.NewServer(&fakeControl{lists: []string{list("")}})
	defer srv.Close()
	t.Setenv("QOOQ_HOME", t.TempDir())
	_ = saveLogin(srv.URL, storedLogin{Token: "revoked", Account: "acme"})
	_, _, err := runProject(t, srv, "ls")
	if err == nil || !strings.Contains(err.Error(), "qooq auth login") {
		t.Fatalf("err=%v", err)
	}
}

func TestAmbiguousNamespaceAndRmNeedsYes(t *testing.T) {
	two := list(proj("success", 1, "2026-01-01T00:00:00Z", "") + "," +
		strings.Replace(proj("success", 1, "2026-01-01T00:00:00Z", ""), `"default"`, `"prod"`, 1))
	srv := httptest.NewServer(&fakeControl{lists: []string{two}})
	defer srv.Close()
	signIn(t, srv)
	_, _, err := runProject(t, srv, "get", "web")
	if err == nil || !strings.Contains(err.Error(), "default, prod") {
		t.Fatalf("err=%v", err)
	}
	out, _, err := runProject(t, srv, "get", "web", "-n", "prod", "-o", "json")
	if err != nil || !strings.Contains(out, `"prod"`) {
		t.Fatalf("out=%s err=%v", out, err)
	}
	if _, _, err := runProject(t, srv, "rm", "web", "-n", "prod"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("rm without --yes: %v", err)
	}
}

// ls and create each have their own --namespace. When they shared a variable,
// create's default ("default") leaked into ls, which then listed nothing for
// an account whose services all live in named namespaces.
func TestListIsNotFilteredByCreatesDefault(t *testing.T) {
	cmd := projectCmd()
	ls, _, err := cmd.Find([]string{"ls"})
	if err != nil {
		t.Fatal(err)
	}
	if v := ls.Flags().Lookup("namespace").Value.String(); v != "" {
		t.Fatalf("ls --namespace defaults to %q, want empty (no filter)", v)
	}
}

func TestExplainDropsTheRouteAndKeepsTheCode(t *testing.T) {
	var o projectOpts
	err := o.explain(&controlclient.APIError{Method: "PUT", Path: "/apis/v1/accounts/a/namespaces/default/projects/q5/source", StatusCode: 409,
		Message: `name_taken: a project named q5 already exists in this account`})
	if got := err.Error(); got != `a project named q5 already exists in this account (name_taken)` {
		t.Errorf("explain = %q", got)
	}
}

// logs is the app's own output; --build is the build's, which is all this
// command used to show.
func TestLogsIsTheAppAndBuildIsTheBuild(t *testing.T) {
	f := &fakeControl{lists: []string{list(proj("success", 3, "", ""))}, buildLog: "Step 1/2 : FROM alpine\n"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	signIn(t, srv)

	out, _, err := runProject(t, srv, "logs", "web", "--tail", "50")
	if err != nil || out != "app says hello\n" {
		t.Fatalf("logs: %q, %v", out, err)
	}
	if len(f.logQueries) != 1 || f.logQueries[0] != "tail=50" {
		t.Errorf("queries = %v", f.logQueries)
	}
	out, _, err = runProject(t, srv, "logs", "web", "--build")
	if err != nil || !strings.Contains(out, "Step 1/2") {
		t.Fatalf("logs --build: %q, %v", out, err)
	}
}
