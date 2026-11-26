package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qooqapp/qooq/pkg/controlclient"
)

// An image service has no branch and no commit. qooq Cloud still
// reports branch "main" and puts the image digest where a git service has its
// commit; printed under "commit:", a digest reads as a sha somebody can look up
// in the repository.
func TestImageServiceShowsADigestNotACommitOrBranch(t *testing.T) {
	p := controlclient.Project{
		Name: "web", Namespace: "default", Source: "image", ImageRef: "nginx:alpine",
		Branch: "main", LastBuildRevision: "sha256:785835e1c0ffee",
	}
	var detail bytes.Buffer
	if err := writeDetail(&detail, p); err != nil {
		t.Fatal(err)
	}
	out := detail.String()
	for _, banned := range []string{"branch:", "commit:"} {
		if strings.Contains(out, banned) {
			t.Errorf("image detail prints %q:\n%s", banned, out)
		}
	}
	// In full: an abbreviated "sha256:785835e" looks like a reference but
	// cannot be pulled.
	if !strings.Contains(out, "digest:") || !strings.Contains(out, "sha256:785835e1c0ffee\n") {
		t.Errorf("image detail does not show the whole digest, labelled as one:\n%s", out)
	}

	var table bytes.Buffer
	if err := writeTable(&table, []controlclient.Project{p}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(table.String(), "COMMIT") || !strings.Contains(table.String(), "sha256:785835e") {
		t.Errorf("list shows a digest as if it were a commit:\n%s", table.String())
	}
}

// A git service keeps both, exactly as before.
func TestGitServiceShowsBranchAndCommit(t *testing.T) {
	p := controlclient.Project{
		Name: "api", Namespace: "default", Source: "git", RepoURL: "https://github.com/me/api",
		Branch: "main", LastBuildRevision: "973bee4425e5eb42",
	}
	var detail bytes.Buffer
	if err := writeDetail(&detail, p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"branch:", "main", "commit:", "973bee4"} {
		if !strings.Contains(detail.String(), want) {
			t.Errorf("git detail is missing %q:\n%s", want, detail.String())
		}
	}
	if strings.Contains(detail.String(), "digest:") {
		t.Errorf("git detail prints a digest row:\n%s", detail.String())
	}
	var table bytes.Buffer
	if err := writeTable(&table, []controlclient.Project{p}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(table.String(), "973bee4") || strings.Contains(table.String(), "sha256:") {
		t.Errorf("git list row is wrong:\n%s", table.String())
	}
}

// After a failed version is rolled back, the previous commit is the one
// serving. Showing only the last build said the broken commit was live.
func TestARolledBackServiceShowsTheCommitThatIsRunning(t *testing.T) {
	p := controlclient.Project{
		Name: "api", Namespace: "default", Source: "git", Branch: "main",
		LastBuildStatus: "failed", LastBuildRevision: "0dbcd78aaaaaaa", DeployedRevision: "aec29dfbbbbbbb",
	}
	var table bytes.Buffer
	if err := writeTable(&table, []controlclient.Project{p}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(table.String(), "aec29df (0dbcd78 failed)") {
		t.Errorf("list does not say which commit runs:\n%s", table.String())
	}
	var detail bytes.Buffer
	if err := writeDetail(&detail, p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail.String(), "commit:") || !strings.Contains(detail.String(), "aec29df") ||
		!strings.Contains(detail.String(), "0dbcd78 failed") {
		t.Errorf("detail does not separate running from last build:\n%s", detail.String())
	}
}

// Before the first deploy the platform's address is shown, marked as not
// answering yet; once there is an endpoint, that is what is shown.
func TestAssignedURLIsShownUntilItAnswers(t *testing.T) {
	p := controlclient.Project{Name: "blog", Namespace: "prod", URL: "https://blog-k3x9q.qooqapp.cloud"}
	var table, detail bytes.Buffer
	if err := writeTable(&table, []controlclient.Project{p}); err != nil {
		t.Fatal(err)
	}
	if err := writeDetail(&detail, p); err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"ls": table.String(), "get": detail.String()} {
		if !strings.Contains(out, p.URL+pendingURL) {
			t.Errorf("%s does not show the assigned URL as pending:\n%s", name, out)
		}
	}
	if !strings.Contains(detail.String(), "url:") {
		t.Errorf("get has no url: line:\n%s", detail.String())
	}

	p.Endpoints = []string{"https://blog-k3x9q.qooqapp.cloud"}
	table.Reset()
	if err := writeTable(&table, []controlclient.Project{p}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(table.String(), pendingURL) {
		t.Errorf("a deployed service is still marked pending:\n%s", table.String())
	}
}
