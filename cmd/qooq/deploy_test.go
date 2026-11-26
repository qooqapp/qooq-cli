package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestNonGitDirectorySkipsTheDefaultsAndEnvFiles(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"index.html": "hi", "css/a.css": "a",
		"node_modules/x/i.js": "x", ".git/config": "c", "sub/node_modules/y.js": "y",
		".venv/bin/p": "p", "__pycache__/m.pyc": "m", ".DS_Store": "d",
		".env": "SECRET=1", ".env.local": "S=2", "sub/.env.production": "S=3",
	})
	set, err := listFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(set.Files, ","), "css/a.css,index.html"; got != want {
		t.Errorf("files = %s, want %s", got, want)
	}
	if got, want := strings.Join(set.EnvSkipped, ","), ".env,.env.local,sub/.env.production"; got != want {
		t.Errorf("env skipped = %s, want %s", got, want)
	}
}

// In a git repository .gitignore decides, untracked files count, and .env is
// still withheld even though nothing ignores it.
func TestGitDirectoryUsesGitignoreAndStillWithholdsEnv(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	writeFiles(t, dir, map[string]string{
		".gitignore": "dist/\n*.log\n",
		"app.js":     "a", "dist/out.js": "o", "debug.log": "l", ".env": "S=1",
		"tracked.txt": "t",
	})
	git(t, dir, "add", "tracked.txt")
	git(t, dir, "commit", "-q", "-m", "x")
	writeFiles(t, dir, map[string]string{"untracked.txt": "u"})
	// A tracked file deleted from the work tree must not break packing.
	os.Remove(filepath.Join(dir, "tracked.txt"))

	set, err := listFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(set.Files, ","), ".gitignore,app.js,untracked.txt"; got != want {
		t.Errorf("files = %s, want %s", got, want)
	}
	if len(set.EnvSkipped) != 1 || set.EnvSkipped[0] != ".env" {
		t.Errorf("env skipped = %v", set.EnvSkipped)
	}
}

func TestQooqignoreAppliesInBothModesIncludingNegation(t *testing.T) {
	body := map[string]string{
		".qooqignore": "# build output\nbuild/\n*.md\n!README.md\n/secrets.txt\ndocs/**/draft.txt\n**/tmp/\n",
		"main.go":     "m", "README.md": "r", "NOTES.md": "n",
		"build/a.o": "o", "secrets.txt": "s", "sub/secrets.txt": "keep",
		"docs/draft.txt": "d1", "docs/x/y/draft.txt": "d2", "docs/keep.txt": "k",
		"a/tmp/f": "t",
	}
	want := ".qooqignore,README.md,docs/keep.txt,main.go,sub/secrets.txt"

	plain := t.TempDir()
	writeFiles(t, plain, body)
	set, err := listFiles(plain)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(set.Files, ","); got != want {
		t.Errorf("non-git: files = %s, want %s", got, want)
	}

	repo := t.TempDir()
	git(t, repo, "init", "-q")
	writeFiles(t, repo, body)
	set, err = listFiles(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(set.Files, ","); got != want {
		t.Errorf("git: files = %s, want %s", got, want)
	}
}

// A file under an ignored directory stays ignored even when a later rule
// re-includes it, as in git.
func TestNegationCannotReachIntoAnIgnoredDirectory(t *testing.T) {
	ir := parseIgnore(strings.NewReader("out/\n!out/keep.txt\n"))
	if !ir.Ignored("out/keep.txt") {
		t.Error("out/keep.txt was re-included from an ignored directory")
	}
}

func TestSymlinksAreKeptAsSymlinks(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "a"})
	if err := os.Symlink("a.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	set, _ := listFiles(dir)
	pk, err := pack(dir, set.Files)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(pk.Path)
	f, _ := os.Open(pk.Path)
	defer f.Close()
	gz, _ := gzip.NewReader(f)
	tr := tar.NewReader(gz)
	found := false
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Name == "link" {
			found = h.Typeflag == tar.TypeSymlink && h.Linkname == "a.txt"
		}
	}
	if !found {
		t.Error("link was not packed as a symlink to a.txt")
	}
}

func TestDeployName(t *testing.T) {
	root := t.TempDir()
	mk := func(dirName string, files map[string]string) string {
		d := filepath.Join(root, dirName)
		os.MkdirAll(d, 0o755)
		writeFiles(t, d, files)
		return d
	}
	cases := []struct {
		desc, dir, flag, want string
		wantErr               string
	}{
		{"flag wins", mk("a", map[string]string{"qooq.yaml": "version: 1\napp: fromyaml\n"}), "chosen", "chosen", ""},
		{"manifest app", mk("b", map[string]string{"qooq.yaml": "version: 1\napp: fromyaml\n"}), "", "fromyaml", ""},
		{"dir name normalized", mk("My Cool_Site", nil), "", "my-cool-site", ""},
		{"unusable dir name", mk("日本語", nil), "", "", "--name"},
		{"bad flag", mk("c", nil), "Bad_Name", "", "not a valid name"},
		{"bad manifest app", mk("d", map[string]string{"qooq.yaml": "version: 1\napp: Not Valid\n"}), "", "", "--name"},
	}
	for _, c := range cases {
		got, err := deployName(c.dir, c.flag)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: err = %v, want containing %q", c.desc, err, c.wantErr)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.desc, got, err, c.want)
		}
	}
}

func TestTooLargeListsTheBiggestFiles(t *testing.T) {
	pk := &packed{Compressed: maxUploadBytes + 1, Largest: []fileSize{{"data/big.bin", 5 << 20}, {"b.bin", 1 << 20}}}
	msg := pk.tooLargeMessage()
	if !strings.Contains(msg, "data/big.bin") || !strings.Contains(msg, "5.0 MB") || !strings.Contains(msg, ".qooqignore") {
		t.Errorf("message = %q", msg)
	}
}

// --- the command, against a fake qooq Cloud server ---

type fakeUploadControl struct {
	mu        sync.Mutex
	existing  string // JSON of the project, or "" for none
	uploads   [][]string
	queued    bool
	gens      []string // successive project lists after the upload
	listN     int
	forceSeen bool
	bodies    []int64
}

func (f *fakeUploadControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/source"):
		f.forceSeen = r.URL.Query().Get("force") == "1"
		f.bodies = append(f.bodies, r.ContentLength)
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		tr := tar.NewReader(gz)
		var names []string
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				w.WriteHeader(400)
				return
			}
			names = append(names, h.Name)
		}
		f.uploads = append(f.uploads, names)
		w.WriteHeader(202)
		if f.queued {
			w.Write([]byte(`{"revision":"abcdef1234","build_queued":true,"build_generation":5,"created":true}`))
		} else {
			w.Write([]byte(`{"revision":"abcdef1234","build_queued":false,"build_generation":5}`))
		}
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/projects"):
		switch {
		case len(f.uploads) == 0:
			if f.existing == "" {
				w.Write([]byte(list("")))
			} else {
				w.Write([]byte(list(f.existing)))
			}
		default:
			i := f.listN
			if i >= len(f.gens) {
				i = len(f.gens) - 1
			}
			f.listN++
			w.Write([]byte(list(f.gens[i])))
		}
	default:
		w.WriteHeader(404)
	}
}

func uploadProj(status string, gen int, at, extra string) string {
	s := `{"name":"site","namespace":"default","source":"upload","last_build_status":"` + status + `","build_generation":` + itoa(gen)
	if at != "" {
		s += `,"last_build_at":"` + at + `"`
	}
	return s + extra + "}"
}

func itoa(n int) string { return strconv.Itoa(n) }

func runDeployCmd(t *testing.T, srv *httptest.Server, dir string, args ...string) (string, string, error) {
	t.Helper()
	pollEvery = 5 * time.Millisecond
	cmd := deployDirCmd()
	var o, e bytes.Buffer
	cmd.SetOut(&o)
	cmd.SetErr(&e)
	// These tests cover the cloud path; deploy_local_test.go covers the default.
	cmd.SetArgs(append([]string{dir, "--cloud", "--addr", srv.URL}, args...))
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.ExecuteContext(context.Background())
	return o.String(), e.String(), err
}

func TestDeployUploadsWithoutEnvAndPrintsTheURL(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.html": "hi", ".env": "S=1", "qooq.yaml": "version: 1\napp: site\n"})
	f := &fakeUploadControl{queued: true, gens: []string{
		uploadProj("building", 6, "", ""),
		uploadProj("success", 6, "2026-11-03T10:00:00Z", `,"endpoints":["http://1.2.3.4:19000"],"phase":"Running"`),
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	signIn(t, srv)

	out, errs, err := runDeployCmd(t, srv, dir)
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, errs)
	}
	if len(f.uploads) != 1 {
		t.Fatalf("uploads = %v", f.uploads)
	}
	for _, n := range f.uploads[0] {
		if strings.HasPrefix(n, ".env") {
			t.Errorf(".env was uploaded: %v", f.uploads[0])
		}
	}
	if f.bodies[0] <= 0 {
		t.Errorf("no Content-Length on the upload: %v", f.bodies)
	}
	if !strings.Contains(errs, "not uploading 1 env file") || !strings.Contains(errs, "http://1.2.3.4:19000") {
		t.Errorf("stderr = %s", errs)
	}
	_ = out
}

// Same content again: exit 0, say so, show the URL, and do not wait for a
// build that is never going to start.
func TestDeploySameContentDoesNotWait(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.html": "hi"})
	cur := uploadProj("success", 5, "2026-11-03T10:00:00Z", `,"endpoints":["http://1.2.3.4:19000"]`)
	f := &fakeUploadControl{queued: false, existing: cur, gens: []string{cur}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	signIn(t, srv)

	done := make(chan error, 1)
	var errs string
	go func() {
		var err error
		_, errs, err = runDeployCmd(t, srv, dir, "--name", "site", "--timeout", "2s")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("deploy: %v\n%s", err, errs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deploy of unchanged content waited for a build")
	}
	if !strings.Contains(errs, "has not changed") || !strings.Contains(errs, "http://1.2.3.4:19000") {
		t.Errorf("stderr = %s", errs)
	}
}

func TestDeployRefusesAnOversizedPackWithoutUploading(t *testing.T) {
	dir := t.TempDir()
	// Incompressible data: random-looking bytes so gzip cannot shrink it.
	big := make([]byte, 3<<20)
	rand.New(rand.NewSource(1)).Read(big)
	os.WriteFile(filepath.Join(dir, "big.bin"), big, 0o644)
	os.WriteFile(filepath.Join(dir, "small.txt"), []byte("x"), 0o644)
	f := &fakeUploadControl{queued: true, gens: []string{uploadProj("success", 6, "2026-11-03T10:00:00Z", "")}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	signIn(t, srv)

	prev := uploadLimit
	uploadLimit = 1 << 20
	defer func() { uploadLimit = prev }()
	_, errs, err := runDeployCmd(t, srv, dir, "--name", "site")
	if err == nil || !strings.Contains(err.Error(), "big.bin") {
		t.Fatalf("err = %v\n%s", err, errs)
	}
	if len(f.uploads) != 0 {
		t.Error("an over-limit pack was uploaded")
	}
}

func TestDeployForceIsPassedThrough(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a": "a"})
	f := &fakeUploadControl{queued: true, gens: []string{uploadProj("success", 6, "2026-11-03T10:00:00Z", "")}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	signIn(t, srv)
	if _, errs, err := runDeployCmd(t, srv, dir, "--name", "site", "--force", "--no-wait"); err != nil || !f.forceSeen {
		t.Fatalf("err=%v force=%v\n%s", err, f.forceSeen, errs)
	}
}

func TestDeployToAGitServiceIsRefusedBeforeUploading(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a": "a"})
	f := &fakeUploadControl{existing: `{"name":"site","namespace":"default","source":"git","last_build_status":"success"}`, gens: []string{"{}"}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	signIn(t, srv)
	_, _, err := runDeployCmd(t, srv, dir, "--name", "site")
	if err == nil || !strings.Contains(err.Error(), "git service") || len(f.uploads) != 0 {
		t.Fatalf("err = %v uploads=%v", err, f.uploads)
	}
}

// A project that compresses well but unpacks huge used to upload in full and
// be refused afterwards, with a message about the compressed size it was under.
func TestDeployRefusesOverTheUnpackedLimitBeforeUploading(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"Dockerfile": "FROM alpine", "data/zeros.txt": strings.Repeat("0", 3<<20)})
	f := &fakeUploadControl{queued: true, gens: []string{uploadProj("success", 6, "2026-11-03T10:00:00Z", "")}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	signIn(t, srv)

	prev := unpackedLimit
	unpackedLimit = 1 << 20
	defer func() { unpackedLimit = prev }()
	_, errs, err := runDeployCmd(t, srv, dir, "--name", "site")
	if err == nil || !strings.Contains(err.Error(), "before compression") || !strings.Contains(err.Error(), "data/zeros.txt") {
		t.Fatalf("err = %v\n%s", err, errs)
	}
	if len(f.uploads) != 0 {
		t.Error("an over-limit directory was uploaded")
	}
}

func TestDeployRefusesTooManyFilesAndSaysWhere(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{"Dockerfile": "FROM alpine"}
	for i := 0; i < 30; i++ {
		files[fmt.Sprintf("vendor/pkg%d.txt", i)] = "x"
	}
	writeFiles(t, dir, files)
	f := &fakeUploadControl{queued: true, gens: []string{uploadProj("success", 6, "2026-11-03T10:00:00Z", "")}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	signIn(t, srv)

	prev := fileCountLimit
	fileCountLimit = 10
	defer func() { fileCountLimit = prev }()
	_, errs, err := runDeployCmd(t, srv, dir, "--name", "site")
	if err == nil || !strings.Contains(err.Error(), "31 files") || !strings.Contains(err.Error(), "30  vendor/") {
		t.Fatalf("err = %v\n%s", err, errs)
	}
	if len(f.uploads) != 0 {
		t.Error("an over-limit directory was uploaded")
	}
}
