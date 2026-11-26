package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSelf stands in for the qooq binary: it prints its working directory,
// its arguments, and the contents of any NAME=@file argument, so a test sees
// exactly what a tool would have run.
func fakeSelf(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "qooq")
	script := `#!/bin/sh
echo "cwd=$(pwd -P)"
for a in "$@"; do
  echo "arg=$a"
  case "$a" in *=@*) echo "file=$(cat "${a#*=@}")" ;; esac
done
[ "$1" = fail ] && exit 3
exit 0
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func mcpSession(t *testing.T, self string, reqs ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	if err := serveMCP(context.Background(), strings.NewReader(strings.Join(reqs, "\n")+"\n"), &out, self); err != nil {
		t.Fatal(err)
	}
	var msgs []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("not JSON on stdout: %q", sc.Text())
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func callText(t *testing.T, m map[string]any) (string, bool) {
	t.Helper()
	r, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", m)
	}
	c := r["content"].([]any)[0].(map[string]any)
	return c["text"].(string), r["isError"].(bool)
}

func TestMCPHandshakeAndToolList(t *testing.T) {
	msgs := mcpSession(t, fakeSelf(t),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"nope"}`,
	)
	if len(msgs) != 3 {
		t.Fatalf("%d replies, want 3 (a notification gets none): %v", len(msgs), msgs)
	}
	init := msgs[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion %v, want the client's own 2025-03-26", init["protocolVersion"])
	}
	tools := msgs[1]["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, x := range tools {
		tool := x.(map[string]any)
		names[tool["name"].(string)] = true
		if tool["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("%v: inputSchema is not an object schema", tool["name"])
		}
	}
	for _, want := range []string{"qooq_deploy", "qooq_list", "qooq_logs", "qooq_build_log", "qooq_secret_import", "qooq_remove"} {
		if !names[want] {
			t.Errorf("tool %s missing", want)
		}
	}
	if msgs[2]["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Errorf("unknown method: %v", msgs[2])
	}
}

func TestMCPDeployRunsInTheProjectDirectory(t *testing.T) {
	dir := t.TempDir()
	msgs := mcpSession(t, fakeSelf(t),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"qooq_deploy","arguments":{"path":"`+dir+`","name":"shop","namespace":"demo","force":true}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"qooq_deploy","arguments":{"path":"relative/dir"}}}`,
	)
	text, isErr := callText(t, msgs[0])
	real, _ := filepath.EvalSymlinks(dir)
	for _, want := range []string{"cwd=" + real, "arg=deploy", "arg=.", "arg=--name\narg=shop", "arg=--force", "arg=-n\narg=demo"} {
		if !strings.Contains(text, want) {
			t.Errorf("deploy ran without %q:\n%s", want, text)
		}
	}
	if isErr {
		t.Error("a zero exit was reported as an error")
	}
	if text, isErr := callText(t, msgs[1]); !isErr || !strings.Contains(text, "absolute") {
		t.Errorf("relative path: isError=%v %q, want a refusal naming absolute paths", isErr, text)
	}
}

// Secret values reach the CLI through private files, never argv, where any
// process on the machine could read them while the command runs.
func TestMCPSecretValuesNeverOnTheCommandLine(t *testing.T) {
	msgs := mcpSession(t, fakeSelf(t),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"qooq_secret_set","arguments":{"values":{"STRIPE_KEY":"sk_test_123"}}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"qooq_set_input","arguments":{"name":"shop","values":{"OPENAI_API_KEY":"abc"}}}}`,
	)
	for i, secret := range []string{"sk_test_123", "abc"} {
		text, _ := callText(t, msgs[i])
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "arg=") && strings.Contains(line, secret) {
				t.Errorf("value on the command line: %s", line)
			}
		}
		if !strings.Contains(text, "file="+secret) {
			t.Errorf("value did not reach the CLI through its file:\n%s", text)
		}
	}
}

func TestMCPFailureIsAToolErrorNotAProtocolError(t *testing.T) {
	// A missing argument and a non-zero exit are results the agent reads and
	// acts on; only an unknown tool is a protocol error.
	msgs := mcpSession(t, fakeSelf(t),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"qooq_logs","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"qooq_nope","arguments":{}}}`,
	)
	if text, isErr := callText(t, msgs[0]); !isErr || !strings.Contains(text, `"name" is required`) {
		t.Errorf("missing name: isError=%v %q", isErr, text)
	}
	if msgs[1]["error"] == nil {
		t.Errorf("unknown tool should be a protocol error: %v", msgs[1])
	}

	self := fakeSelf(t)
	tool := mcpTool{args: func(mcpArgs) ([]string, string, func(), error) { return []string{"fail"}, "", nil, nil }}
	if _, isErr := runTool(context.Background(), self, tool, mcpArgs{}); !isErr {
		t.Error("a non-zero exit was not reported as isError")
	}
}

func TestMCPInstallKeepsTheAgentsOtherSettings(t *testing.T) {
	dir := t.TempDir()
	cursor := filepath.Join(dir, "mcp.json")
	os.WriteFile(cursor, []byte(`{"mcpServers":{"other":{"command":"x"}},"theme":"dark"}`), 0o644)
	if err := addCursorServer(cursor, "/opt/qooq"); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	data, _ := os.ReadFile(cursor)
	json.Unmarshal(data, &cfg)
	servers := cfg["mcpServers"].(map[string]any)
	if servers["other"] == nil || cfg["theme"] != "dark" {
		t.Fatalf("existing settings lost: %s", data)
	}
	if servers["qooq"].(map[string]any)["command"] != "/opt/qooq" {
		t.Fatalf("qooq not added: %s", data)
	}

	os.WriteFile(cursor, []byte(`{broken`), 0o644)
	if err := addCursorServer(cursor, "/opt/qooq"); err == nil {
		t.Fatal("a file that is not JSON was overwritten")
	}

	codex := filepath.Join(dir, "config.toml")
	os.WriteFile(codex, []byte("model = \"o3\""), 0o644)
	if added, err := addCodexServer(codex, "/opt/qooq"); err != nil || !added {
		t.Fatalf("added=%v err=%v", added, err)
	}
	if added, _ := addCodexServer(codex, "/opt/qooq"); added {
		t.Fatal("second install appended a duplicate table")
	}
	data, _ = os.ReadFile(codex)
	want := "model = \"o3\"\n\n[mcp_servers.qooq]\ncommand = \"/opt/qooq\"\nargs = [\"mcp\"]\n"
	if string(data) != want {
		t.Fatalf("config.toml:\n%s\nwant:\n%s", data, want)
	}
}
