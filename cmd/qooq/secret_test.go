package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestParseDotenv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	body := "# keys\n\nexport STRIPE_KEY=sk_test_1\nDB_URL=\"postgres://a:b@h/db\"\nEMPTY=\nQUOTED='x y'\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := parseDotenv(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"STRIPE_KEY": "sk_test_1", "DB_URL": "postgres://a:b@h/db", "EMPTY": "", "QUOTED": "x y"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q", k, got[k], v)
		}
	}

	if err := os.WriteFile(p, []byte("1BAD=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseDotenv(p); err == nil || !strings.Contains(err.Error(), ".env:1") {
		t.Fatalf("err = %v, want it to name the file and line", err)
	}
}

func TestSecretCommandsTalkToTheNamespaceStore(t *testing.T) {
	var mu sync.Mutex
	store := map[string]string{}
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.Method {
		case http.MethodPut:
			var b struct{ Value string }
			_ = json.NewDecoder(r.Body).Decode(&b)
			store[filepath.Base(r.URL.Path)] = b.Value
			_, _ = w.Write([]byte(`{}`))
		case http.MethodGet:
			names := []string{}
			for n := range store {
				names = append(names, n)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"names": names})
		case http.MethodDelete:
			delete(store, filepath.Base(r.URL.Path))
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	saved := watcherAddr
	t.Cleanup(func() { watcherAddr = saved })

	run := func(args ...string) (string, error) {
		cmd := secretCmd()
		var out, errs bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errs)
		cmd.SetArgs(append(args, "--watcher-addr", srv.URL))
		err := cmd.ExecuteContext(context.Background())
		return out.String() + errs.String(), err
	}

	if _, err := run("set", "API_KEY=abc", "-n", "shop"); err != nil {
		t.Fatal(err)
	}
	if store["API_KEY"] != "abc" || paths[0] != "PUT /v1/namespaces/shop/secrets/API_KEY" {
		t.Fatalf("store %v, requests %v", store, paths)
	}
	if out, err := run("ls", "-n", "shop"); err != nil || !strings.Contains(out, "API_KEY") || strings.Contains(out, "abc") {
		t.Fatalf("ls: %q %v — want the name and never the value", out, err)
	}
	if _, err := run("set", "bad-name=x"); err == nil {
		t.Fatal("a name that ${secret.NAME} cannot read was accepted")
	}
	if _, err := run("rm", "API_KEY", "-n", "shop"); err != nil || len(store) != 0 {
		t.Fatalf("rm: %v, store %v", err, store)
	}
}
