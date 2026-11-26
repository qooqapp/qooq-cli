package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

// `qooq mcp`: a Model Context Protocol server on stdin/stdout, so an AI agent
// (Claude Code, Cursor, Codex…) gets qooq as typed tools instead of having to
// learn the CLI from a skill.
//
// Every tool runs this same binary as a subprocess with the matching CLI
// arguments. That is deliberate: the CLI already holds the behaviour (waiting
// for builds, explaining errors, picking addresses), and stdout is the
// protocol channel here, so running a command in-process would write its
// human output into the JSON stream.
//
// The protocol is small enough to speak directly — newline-delimited JSON-RPC
// 2.0, three methods that matter — so there is no SDK dependency.

// mcpProtocolVersions are the revisions this server speaks. A client asking
// for one of them gets it back; anything else gets the newest.
var mcpProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`

	// args builds the CLI arguments from the call's arguments; dir is the
	// working directory to run in ("" = inherit), cleanup runs afterwards.
	args    func(a mcpArgs) (args []string, dir string, cleanup func(), err error) `json:"-"`
	timeout time.Duration                                                          `json:"-"`
}

type mcpArgs map[string]any

func (a mcpArgs) str(k string) string {
	s, _ := a[k].(string)
	return s
}

func (a mcpArgs) need(k string) (string, error) {
	s := a.str(k)
	if s == "" {
		return "", fmt.Errorf("%q is required", k)
	}
	return s, nil
}

func (a mcpArgs) boolean(k string) bool {
	b, _ := a[k].(bool)
	return b
}

func (a mcpArgs) namespace(args []string) []string {
	if ns := a.str("namespace"); ns != "" {
		return append(args, "-n", ns)
	}
	return args
}

func schema(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }

var nameProp = prop("string", "service name, as shown by qooq_list")
var nsProp = prop("string", "namespace (default: \"default\")")

func mcpTools() []mcpTool {
	simple := func(cli ...string) func(mcpArgs) ([]string, string, func(), error) {
		return func(mcpArgs) ([]string, string, func(), error) { return cli, "", nil, nil }
	}
	named := func(cli ...string) func(mcpArgs) ([]string, string, func(), error) {
		return func(a mcpArgs) ([]string, string, func(), error) {
			n, err := a.need("name")
			return append(append([]string{}, cli...), n), "", nil, err
		}
	}
	return []mcpTool{
		{
			Name: "qooq_status",
			Description: "Check whether qooq is running on this machine (its two daemons, qooqd and qooq-watcher). " +
				"Call this first when a deploy fails to connect; start it with qooq_up.",
			InputSchema: schema(nil, map[string]any{}),
			args:        simple("status"),
		},
		{
			Name:        "qooq_up",
			Description: "Start qooq on this machine in the background (safe to call when it is already running). Needs Docker running.",
			InputSchema: schema(nil, map[string]any{}),
			args:        simple("up"),
			timeout:     2 * time.Minute,
		},
		{
			Name: "qooq_deploy",
			Description: "Build and deploy a project directory on this machine and wait until it is running; returns the URLs it is reachable at. " +
				"No Dockerfile or config is needed: the stack (Next.js, Vite, Express, FastAPI, Flask, Django, Go, …) is detected from the files; " +
				"a qooq.yaml in the directory is used when present. Deploying the same directory again updates it, and unchanged content is not rebuilt. " +
				"On failure the result includes the end of the build output; qooq_build_log has all of it. " +
				"Keys from a .env file are not uploaded — store them with qooq_secret_import first.",
			InputSchema: schema([]string{"path"}, map[string]any{
				"path":      prop("string", "absolute path of the project directory"),
				"name":      prop("string", "service name (default: qooq.yaml's app, else the directory name)"),
				"namespace": nsProp,
				"force":     prop("boolean", "rebuild even if nothing changed"),
			}),
			args: func(a mcpArgs) ([]string, string, func(), error) {
				dir, err := a.need("path")
				if err != nil {
					return nil, "", nil, err
				}
				if !filepath.IsAbs(dir) {
					return nil, "", nil, fmt.Errorf("path must be absolute, got %q", dir)
				}
				if st, err := os.Stat(dir); err != nil || !st.IsDir() {
					return nil, "", nil, fmt.Errorf("%s is not a directory", dir)
				}
				args := []string{"deploy", "."}
				if n := a.str("name"); n != "" {
					args = append(args, "--name", n)
				}
				if a.boolean("force") {
					args = append(args, "--force")
				}
				return a.namespace(args), dir, nil, nil
			},
			timeout: 20 * time.Minute,
		},
		{
			Name:        "qooq_list",
			Description: "List deployed services with their status, last deploy result and URLs.",
			InputSchema: schema(nil, map[string]any{}),
			args:        simple("watch", "ls"),
		},
		{
			Name: "qooq_get",
			Description: "Details of one service: URLs, last build status and error, inputs it is still missing, " +
				"and the exact command to supply them.",
			InputSchema: schema([]string{"name"}, map[string]any{"name": nameProp}),
			args:        named("watch", "get"),
		},
		{
			Name:        "qooq_logs",
			Description: "The running application's recent output (stdout and stderr). Use it when the app is deployed but misbehaves or keeps restarting.",
			InputSchema: schema([]string{"name"}, map[string]any{
				"name": nameProp,
				"tail": prop("integer", "how many lines from the end (default 200)"),
			}),
			args: func(a mcpArgs) ([]string, string, func(), error) {
				n, err := a.need("name")
				tail := 200
				if t, ok := a["tail"].(float64); ok && t > 0 {
					tail = int(t)
				}
				return []string{"watch", "logs", n, "--tail", strconv.Itoa(tail)}, "", nil, err
			},
		},
		{
			Name:        "qooq_build_log",
			Description: "The full output of a service's most recent build. Use it when a deploy failed while building.",
			InputSchema: schema([]string{"name"}, map[string]any{"name": nameProp}),
			args:        named("watch", "build-log"),
		},
		{
			Name: "qooq_redeploy",
			Description: "Ask for a service to be rebuilt and redeployed from its current source without waiting for it; " +
				"follow up with qooq_get. To deploy changed local files, call qooq_deploy instead.",
			InputSchema: schema([]string{"name"}, map[string]any{
				"name":  nameProp,
				"force": prop("boolean", "rebuild even if the source has not changed"),
			}),
			args: func(a mcpArgs) ([]string, string, func(), error) {
				n, err := a.need("name")
				args := []string{"watch", "trigger", n}
				if a.boolean("force") {
					args = append(args, "--force")
				}
				return args, "", nil, err
			},
		},
		{
			Name:        "qooq_stop",
			Description: "Stop a service's process. It stays stopped until qooq_start; nothing is deleted.",
			InputSchema: schema([]string{"name"}, map[string]any{"name": nameProp}),
			args:        named("watch", "stop"),
		},
		{
			Name:        "qooq_start",
			Description: "Start a service that was stopped with qooq_stop.",
			InputSchema: schema([]string{"name"}, map[string]any{"name": nameProp}),
			args:        named("watch", "start"),
		},
		{
			Name: "qooq_remove",
			Description: "Permanently remove a service together with its data, including databases it declared. " +
				"Cannot be undone: confirm with the user before calling, and never call it just to redeploy.",
			InputSchema: schema([]string{"name"}, map[string]any{"name": nameProp}),
			args:        named("watch", "rm"),
		},
		{
			Name: "qooq_set_input",
			Description: "Supply values for the inputs a service's qooq.yaml declares (shown as missing by qooq_get); " +
				"the service redeploys right away. An empty value removes one.",
			InputSchema: schema([]string{"name", "values"}, map[string]any{
				"name":   nameProp,
				"values": map[string]any{"type": "object", "description": "input name → value", "additionalProperties": map[string]any{"type": "string"}},
			}),
			args: func(a mcpArgs) ([]string, string, func(), error) {
				n, err := a.need("name")
				if err != nil {
					return nil, "", nil, err
				}
				vals, _ := a["values"].(map[string]any)
				if len(vals) == 0 {
					return nil, "", nil, fmt.Errorf(`"values" needs at least one entry`)
				}
				// Values go through files, not argv: a secret on a command line
				// is readable by every process on the machine while it runs.
				pairs, cleanup, err := valuesViaFiles(vals)
				return append([]string{"watch", "set-input", n}, pairs...), "", cleanup, err
			},
		},
		{
			Name: "qooq_secret_set",
			Description: "Store secrets (API keys, tokens) that qooq.yaml reads as ${secret.NAME}. " +
				"Values are never shown again. Redeploy afterwards to pick up a changed value.",
			InputSchema: schema([]string{"values"}, map[string]any{
				"values":    map[string]any{"type": "object", "description": "secret name (like STRIPE_KEY) → value", "additionalProperties": map[string]any{"type": "string"}},
				"namespace": nsProp,
			}),
			args: func(a mcpArgs) ([]string, string, func(), error) {
				vals, _ := a["values"].(map[string]any)
				if len(vals) == 0 {
					return nil, "", nil, fmt.Errorf(`"values" needs at least one entry`)
				}
				pairs, cleanup, err := valuesViaFiles(vals)
				return a.namespace(append([]string{"secret", "set"}, pairs...)), "", cleanup, err
			},
		},
		{
			Name: "qooq_secret_import",
			Description: "Store every KEY=VALUE in a project's .env file as a secret, so the app's keys reach the deployed service " +
				"(.env itself is never uploaded). Reference them in qooq.yaml as ${secret.KEY}.",
			InputSchema: schema([]string{"path"}, map[string]any{
				"path":      prop("string", "absolute path of the .env file"),
				"namespace": nsProp,
			}),
			args: func(a mcpArgs) ([]string, string, func(), error) {
				p, err := a.need("path")
				return a.namespace([]string{"secret", "import", p}), "", nil, err
			},
		},
		{
			Name:        "qooq_secret_list",
			Description: "List the names of stored secrets (never their values).",
			InputSchema: schema(nil, map[string]any{"namespace": nsProp}),
			args: func(a mcpArgs) ([]string, string, func(), error) {
				return a.namespace([]string{"secret", "ls"}), "", nil, nil
			},
		},
	}
}

// valuesViaFiles writes each value to a private temp file and returns
// NAME=@file arguments, which both `secret set` and `watch set-input` read.
func valuesViaFiles(vals map[string]any) ([]string, func(), error) {
	dir, err := os.MkdirTemp("", "qooq-mcp-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	var pairs []string
	i := 0
	for k, v := range vals {
		s, ok := v.(string)
		if !ok {
			cleanup()
			return nil, nil, fmt.Errorf("value of %q must be a string", k)
		}
		if s == "" {
			pairs = append(pairs, k+"=")
			continue
		}
		f := filepath.Join(dir, strconv.Itoa(i))
		i++
		if err := os.WriteFile(f, []byte(s), 0o600); err != nil {
			cleanup()
			return nil, nil, err
		}
		pairs = append(pairs, k+"=@"+f)
	}
	return pairs, cleanup, nil
}

// maxToolOutput bounds what one call hands back to the agent. The end is kept:
// that is where a build log says why it failed.
const maxToolOutput = 48 << 10

func runTool(ctx context.Context, self string, t mcpTool, a mcpArgs) (string, bool) {
	args, dir, cleanup, err := t.args(a)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return err.Error(), true
	}
	timeout := t.timeout
	if timeout == 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	text := out.String()
	if len(text) > maxToolOutput {
		text = "[…earlier output cut…]\n" + text[len(text)-maxToolOutput:]
	}
	if ctx.Err() == context.DeadlineExceeded {
		return text + fmt.Sprintf("\n(qooq %s did not finish within %s)", args[0], timeout), true
	}
	if text == "" && err == nil {
		text = "done"
	}
	return text, err != nil
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// serveMCP answers requests from in on out until in closes. Calls run one at
// a time: a deploy holds the line, which is what an agent waiting on it wants,
// and it keeps the output ordering trivially correct.
func serveMCP(ctx context.Context, in io.Reader, out io.Writer, self string) error {
	tools := mcpTools()
	byName := map[string]mcpTool{}
	for _, t := range tools {
		byName[t.Name] = t
	}
	enc := json.NewEncoder(out)
	reply := func(id json.RawMessage, result any, rerr *rpcError) error {
		msg := map[string]any{"jsonrpc": "2.0", "id": id}
		if rerr != nil {
			msg["error"] = rerr
		} else {
			msg["result"] = result
		}
		return enc.Encode(msg)
	}

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m rpcMessage
		if err := json.Unmarshal(line, &m); err != nil {
			if err := reply(json.RawMessage("null"), nil, &rpcError{-32700, "parse error"}); err != nil {
				return err
			}
			continue
		}
		if len(m.ID) == 0 {
			continue // a notification (initialized, cancelled…): nothing to answer
		}
		var err error
		switch m.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			json.Unmarshal(m.Params, &p)
			v := mcpProtocolVersions[0]
			for _, s := range mcpProtocolVersions {
				if s == p.ProtocolVersion {
					v = s
				}
			}
			err = reply(m.ID, map[string]any{
				"protocolVersion": v,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "qooq", "version": version},
				"instructions": "qooq deploys projects on this machine and returns working URLs. " +
					"Typical flow: qooq_status (qooq_up if it is not running) → qooq_deploy with the project's absolute path → " +
					"open the returned URL to confirm. When a deploy fails, read qooq_build_log; when it runs but misbehaves, qooq_logs.",
			}, nil)
		case "ping":
			err = reply(m.ID, map[string]any{}, nil)
		case "tools/list":
			err = reply(m.ID, map[string]any{"tools": tools}, nil)
		case "tools/call":
			var p struct {
				Name      string  `json:"name"`
				Arguments mcpArgs `json:"arguments"`
			}
			if jerr := json.Unmarshal(m.Params, &p); jerr != nil {
				err = reply(m.ID, nil, &rpcError{-32602, "invalid params"})
				break
			}
			t, ok := byName[p.Name]
			if !ok {
				err = reply(m.ID, nil, &rpcError{-32602, "unknown tool " + p.Name})
				break
			}
			if p.Arguments == nil {
				p.Arguments = mcpArgs{}
			}
			text, isErr := runTool(ctx, self, t, p.Arguments)
			err = reply(m.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": isErr,
			}, nil)
		default:
			err = reply(m.ID, nil, &rpcError{-32601, "method not found: " + m.Method})
		}
		if err != nil {
			return err
		}
	}
	return sc.Err()
}

func mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP server so AI agents can deploy with qooq as tools",
		Long: `Run a Model Context Protocol server on stdin/stdout. AI agents that support MCP
then get qooq as tools — deploy a directory, list services, read logs and build
output, store secrets — instead of having to drive the CLI.

Register it once with your agent:

  qooq mcp install                  # Claude Code
  qooq mcp install --agent cursor   # Cursor (~/.cursor/mcp.json)
  qooq mcp install --agent codex    # Codex ($CODEX_HOME/config.toml)

It talks to the qooq on this machine (QOOQ_ADDR / QOOQ_WATCHER_ADDR apply).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			self, err := os.Executable()
			if err != nil {
				return err
			}
			return serveMCP(cmd.Context(), os.Stdin, os.Stdout, self)
		},
	}
	cmd.AddCommand(mcpInstallCmd())
	return cmd
}

// mcpInstallCmd registers `qooq mcp` with an agent, so nobody has to find and
// hand-edit that agent's config file. The binary is named by absolute path:
// an agent started from a dock or an IDE often has a shorter PATH than the
// shell qooq was installed from.
func mcpInstallCmd() *cobra.Command {
	var agent string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Register the qooq MCP server with an AI agent (claude, cursor, codex)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			self, err := os.Executable()
			if err != nil {
				return err
			}
			if resolved, err := filepath.EvalSymlinks(self); err == nil {
				self = resolved
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			w := cmd.ErrOrStderr()
			switch agent {
			case "claude":
				manual := fmt.Sprintf("claude mcp add --scope user qooq -- %s mcp", self)
				claude, err := exec.LookPath("claude")
				if err != nil {
					fmt.Fprintf(w, "the claude command is not on PATH; run this where it is:\n  %s\n", manual)
					return nil
				}
				c := exec.CommandContext(cmd.Context(), claude, "mcp", "add", "--scope", "user", "qooq", "--", self, "mcp")
				c.Stdout, c.Stderr = w, w
				if err := c.Run(); err != nil {
					return fmt.Errorf("%s: %w (already added? check with: claude mcp list)", manual, err)
				}
			case "cursor":
				path := filepath.Join(home, ".cursor", "mcp.json")
				if err := addCursorServer(path, self); err != nil {
					return err
				}
				fmt.Fprintf(w, "added qooq to %s\n", path)
			case "codex":
				dir := filepath.Join(home, ".codex")
				if h := os.Getenv("CODEX_HOME"); h != "" {
					dir = h
				}
				path := filepath.Join(dir, "config.toml")
				added, err := addCodexServer(path, self)
				if err != nil {
					return err
				}
				if !added {
					fmt.Fprintf(w, "%s already has [mcp_servers.qooq]; left it as it is\n", path)
					return nil
				}
				fmt.Fprintf(w, "added [mcp_servers.qooq] to %s\n", path)
			default:
				return fmt.Errorf("--agent %q: use claude, cursor or codex", agent)
			}
			fmt.Fprintln(w, "start a new agent session; it can now deploy with the qooq_* tools")
			return nil
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "claude", "which agent to register with: claude, cursor or codex")
	return cmd
}

// addCursorServer sets mcpServers.qooq in Cursor's mcp.json, keeping every
// other server and setting that is already there.
func addCursorServer(path, self string) error {
	cfg := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("%s is not valid JSON, so it was not changed: %w", path, err)
		}
	case !os.IsNotExist(err):
		return err
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers["qooq"] = map[string]any{"command": self, "args": []string{"mcp"}}
	cfg["mcpServers"] = servers
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// addCodexServer appends an [mcp_servers.qooq] table to Codex's config.toml.
// An existing one is left alone: rewriting TOML without a parser would risk
// the rest of the user's file.
func addCodexServer(path, self string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if bytes.Contains(data, []byte("[mcp_servers.qooq]")) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	var b bytes.Buffer
	b.Write(data)
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		b.WriteByte('\n')
	}
	if len(data) > 0 {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "[mcp_servers.qooq]\ncommand = %s\nargs = [\"mcp\"]\n", strconv.Quote(self))
	return true, os.WriteFile(path, b.Bytes(), 0o644)
}
