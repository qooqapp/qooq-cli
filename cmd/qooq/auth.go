package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/qooqapp/qooq/pkg/controlclient"
)

// Signing in to qooq Cloud.
//
// The people this is for do not necessarily have a git host account or any
// interest in copying a token out of a web page, so the default is the flow
// every other developer CLI uses: a browser opens, they click the provider they
// already use, and the terminal says who they are.
//
// This is the only group of commands that talks to qooq Cloud. qooqd and
// the watcher know nothing about accounts, so none of the rest needs it — and
// running qooq on your own machine never involves signing in at all.

// defaultControlAddr matches the rest of the CLI's defaults, which all point at
// a daemon on this machine. There is no hosted address to default to yet; when
// there is, this is the one line that changes.
const defaultControlAddr = "http://127.0.0.1:8080"

// loginTimeout bounds how long the listener waits. Long enough to create an
// account with a provider on the way through, short enough that an abandoned
// login does not leave a port open all afternoon.
const loginTimeout = 5 * time.Minute

func authCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "Sign in to qooq Cloud"}

	var addr, account string
	var withToken bool

	login := &cobra.Command{
		Use:   "login",
		Short: "Sign in through your browser",
		Long: "Opens a page where you pick Google, GitHub or whichever provider\n" +
			"qooq Cloud offers, and stores the token it issues.\n\n" +
			"On a machine with no browser — a server over SSH, a container —\n" +
			"use --with-token and paste a token from the console instead.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if withToken {
				return loginWithToken(cmd.Context(), controlAddr(addr), account)
			}
			return loginViaBrowser(cmd.Context(), controlAddr(addr), account)
		},
	}
	login.Flags().StringVar(&addr, "control-addr", "", "qooq Cloud address (default: $QOOQ_CONTROL_ADDR, else "+defaultControlAddr+" for a local development server)")
	login.Flags().StringVar(&account, "account", "", "account to issue the token for (default: your personal account)")
	login.Flags().BoolVar(&withToken, "with-token", false, "read a token from stdin instead of opening a browser (needs --account)")
	cmd.AddCommand(login)

	status := &cobra.Command{
		Use:   "status",
		Short: "Show who this machine is signed in as",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return authStatus(cmd.Context(), controlAddr(addr))
		},
	}
	status.Flags().StringVar(&addr, "control-addr", "", "qooq Cloud address")
	cmd.AddCommand(status)

	logout := &cobra.Command{
		Use:   "logout",
		Short: "Forget the stored token",
		Long: "Removes the token from this machine. It stays valid until it is\n" +
			"revoked on the console's API token page — this is not a revocation.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return authLogout(controlAddr(addr))
		},
	}
	logout.Flags().StringVar(&addr, "control-addr", "", "qooq Cloud address")
	cmd.AddCommand(logout)

	return cmd
}

func controlAddr(flag string) string {
	if flag != "" {
		return strings.TrimRight(flag, "/")
	}
	if e := os.Getenv("QOOQ_CONTROL_ADDR"); e != "" {
		return strings.TrimRight(e, "/")
	}
	return defaultControlAddr
}

// --- the browser flow ---

func loginViaBrowser(ctx context.Context, addr, account string) error {
	verifier, err := randomSecret()
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	// Listen before opening the browser: the page's links already name this
	// port, so a listener that fails to come up afterwards would send somebody
	// through a sign-in that had nowhere to land.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("cannot listen on localhost for the sign-in to come back to: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	startURL := fmt.Sprintf("%s/auth/cli/start?port=%d&challenge=%s", addr, port, challenge)
	if account != "" {
		startURL += "&account=" + account
	}

	results := make(chan callbackResult, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res := callbackResult{code: r.URL.Query().Get("code"), refused: r.URL.Query().Get("error")}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch {
		case res.refused != "":
			w.WriteHeader(http.StatusForbidden)
			title, note := refusalPage(res.refused, addr)
			_, _ = w.Write([]byte(browserPage(title, note)))
		case res.code == "":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(browserPage("登入沒有完成", "回到終端機再試一次。")))
			return
		default:
			_, _ = w.Write([]byte(browserPage("已經登入", "可以關掉這一頁，回到終端機。")))
		}
		select {
		case results <- res:
		default:
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	fmt.Fprintf(os.Stderr, "在瀏覽器裡完成登入：\n  %s\n\n", startURL)
	if err := openBrowser(startURL); err != nil {
		fmt.Fprintln(os.Stderr, "（開不了瀏覽器，請自己複製上面的網址）")
	}

	// Ctrl-C has to work: this waits for minutes, and a command that cannot be
	// interrupted is one people kill with -9 and then wonder about the port.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	var code string
	select {
	case res := <-results:
		if res.refused != "" {
			return refusalError(res.refused, addr)
		}
		code = res.code
	case <-time.After(loginTimeout):
		return fmt.Errorf("timed out after %s waiting for the browser", loginTimeout)
	case <-ctx.Done():
		return fmt.Errorf("cancelled")
	}

	client := controlclient.New(addr, controlclient.WithUserAgent(cliUserAgent()))
	out, err := client.ExchangeCLIToken(context.Background(), code, verifier)
	if err != nil {
		return err
	}
	if err := saveLogin(addr, storedLogin{
		Token: out.Token, Account: out.Account, User: out.User, SignedInAt: time.Now().UTC(),
	}); err != nil {
		return err
	}

	fmt.Printf("signed in as %s -> account %s\n", out.User, out.Account)
	fmt.Printf("token stored in %s\n", mustAuthPath())
	return nil
}

// loginWithToken is the escape hatch for a machine with no browser.
//
// The account has to be named: a token knows which account it belongs to, but
// there is no route that will tell its holder, and storing one unverified would
// mean the first real command is where the mistake surfaces.
func loginWithToken(ctx context.Context, addr, account string) error {
	if account == "" {
		return fmt.Errorf("--with-token needs --account: say which account the token belongs to")
	}
	fmt.Fprint(os.Stderr, "貼上 token（console 的 API token 頁面可以產生），然後按 Enter：\n")
	var token string
	if _, err := fmt.Fscanln(os.Stdin, &token); err != nil {
		return fmt.Errorf("no token read from stdin: %w", err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("no token read from stdin")
	}

	client := controlclient.New(addr, controlclient.WithToken(token))
	acc, err := client.Account(ctx, account)
	if err != nil {
		return fmt.Errorf("that token does not work for account %q: %w", account, err)
	}
	if err := saveLogin(addr, storedLogin{Token: token, Account: acc.Slug, SignedInAt: time.Now().UTC()}); err != nil {
		return err
	}
	fmt.Printf("token accepted for account %s\n", acc.Slug)
	return nil
}

func authStatus(ctx context.Context, addr string) error {
	login, ok, err := loadLogin(addr)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("not signed in to %s\n", addr)
		fmt.Println("run: qooq auth login")
		return nil
	}

	client := controlclient.New(addr, controlclient.WithToken(login.Token))
	acc, err := client.Account(ctx, login.Account)
	if err != nil {
		// Say which it is. "Signed in" next to a token qooq Cloud has
		// revoked is worse than saying nothing.
		if code := controlclient.StatusCode(err); code == http.StatusUnauthorized || code == http.StatusForbidden {
			fmt.Printf("%s: the stored token is no longer accepted (account %s)\n", addr, login.Account)
			fmt.Println("run: qooq auth login")
			return nil
		}
		return err
	}

	name := acc.DisplayName
	if name == "" {
		name = acc.Slug
	}
	fmt.Printf("%s\n", addr)
	fmt.Printf("  account  %s (%s, %s)\n", acc.Slug, name, acc.Kind)
	if login.User != "" {
		fmt.Printf("  signed in as %s\n", login.User)
	}
	fmt.Printf("  since    %s\n", login.SignedInAt.Local().Format(time.RFC3339))
	return nil
}

func authLogout(addr string) error {
	store, err := loadStore()
	if err != nil {
		return err
	}
	// Every spelling, not just this one: leaving one behind means the next
	// command is signed in again as somebody.
	key := controlKey(addr)
	removed := 0
	for k := range store.Logins {
		if controlKey(k) == key {
			delete(store.Logins, k)
			removed++
		}
	}
	if removed == 0 {
		fmt.Printf("not signed in to %s\n", addr)
		return nil
	}
	if err := saveStore(store); err != nil {
		return err
	}
	fmt.Printf("forgot the token for %s\n", addr)
	fmt.Println("it is still valid until you revoke it on the console's API token page")
	return nil
}

// --- where the token lives ---

// storedLogin is one qooq Cloud server's credential.
type storedLogin struct {
	Token      string    `json:"token"`
	Account    string    `json:"account"`
	User       string    `json:"user,omitempty"`
	SignedInAt time.Time `json:"signed_in_at"`
}

// authStore is keyed by qooq Cloud address.
//
// A map rather than one record because signing in somewhere else must not
// silently drop the credential for the first — somebody with a local development server
// and qooq Cloud would otherwise be re-authenticating every
// time they switch.
type authStore struct {
	Logins map[string]storedLogin `json:"logins"`
}

func authPath() (string, error) {
	home, err := qooqHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "auth.json"), nil
}

func mustAuthPath() string {
	p, err := authPath()
	if err != nil {
		return "~/.qooq/auth.json"
	}
	return p
}

func loadStore() (authStore, error) {
	store := authStore{Logins: map[string]storedLogin{}}
	path, err := authPath()
	if err != nil {
		return store, err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		return store, err
	}
	if err := json.Unmarshal(raw, &store); err != nil {
		return store, fmt.Errorf("%s is not readable as JSON; move it aside and sign in again: %w", path, err)
	}
	if store.Logins == nil {
		store.Logins = map[string]storedLogin{}
	}
	return store, nil
}

func saveStore(store authStore) error {
	path, err := authPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	// 0600, and written through a temporary file: a token half-written over an
	// old one leaves a file that is neither credential.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func saveLogin(addr string, l storedLogin) error {
	store, err := loadStore()
	if err != nil {
		return err
	}
	key := controlKey(addr)
	// Drop the other spellings rather than leave them behind: a stale
	// credential for the same qooq Cloud server is one a later lookup can still
	// choose.
	for k := range store.Logins {
		if k != key && controlKey(k) == key {
			delete(store.Logins, k)
		}
	}
	store.Logins[key] = l
	return saveStore(store)
}

func loadLogin(addr string) (storedLogin, bool, error) {
	store, err := loadStore()
	if err != nil {
		return storedLogin{}, false, err
	}
	l, ok := findLogin(store, addr)
	return l, ok, nil
}

// findLogin accepts any spelling, including the one an older qooq wrote when it
// stored the address exactly as typed.
//
// The most recent sign-in wins. A file can hold two spellings of one control
// plane -- every file written before this did -- and ranging a map would return
// whichever Go picked, so the credential would work on some runs and not on
// others.
func findLogin(store authStore, addr string) (storedLogin, bool) {
	key := controlKey(addr)
	var best storedLogin
	found := false
	for k, l := range store.Logins {
		if controlKey(k) != key {
			continue
		}
		if !found || l.SignedInAt.After(best.SignedInAt) {
			best, found = l, true
		}
	}
	return best, found
}

// --- small helpers ---

// cliUserAgent is what qooq Cloud names the token after.
func cliUserAgent() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "qooq-cli/" + version
	}
	// Strip the mDNS suffix a Mac reports, which is noise in a token list.
	host = strings.TrimSuffix(host, ".local")
	return "qooq-cli/" + version + " (" + host + ")"
}

func randomSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// openBrowser is best effort. The URL is printed first either way, so a failure
// here costs a copy and paste rather than the login.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

// callbackResult is what the browser brought back: a code to exchange, or the
// identifier of the reason qooq Cloud would not issue one.
type callbackResult struct {
	code    string
	refused string
}

// refusalPage is the browser tab's wording for a refusal. The identifier comes
// from a redirect anyone can forge, so it only selects wording written here and
// is never itself put on the page.
func refusalPage(refused, addr string) (title, note string) {
	if refused == "access_code_required" {
		console := html.EscapeString(strings.TrimRight(addr, "/") + "/")
		return "帳號還沒啟用",
			`先到 <a style="color:#8ab4f8" href="` + console + `">qooq console</a> 輸入收到的推薦碼，` +
				`再回終端機重新執行 <code>qooq auth login</code>。`
	}
	return "登入被拒絕", "原因寫在終端機裡。"
}

// refusalError is the terminal's wording for the same refusal.
func refusalError(refused, addr string) error {
	if refused == "access_code_required" {
		return fmt.Errorf("this account has not been activated yet: open %s/, redeem your early-access code, then run `qooq auth login` again",
			strings.TrimRight(addr, "/"))
	}
	return fmt.Errorf("qooq Cloud refused this sign-in (%s)", sanitizeRefusal(refused))
}

// sanitizeRefusal keeps a forged identifier from writing control characters to
// the terminal.
func sanitizeRefusal(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '?'
	}, s)
}

func browserPage(title, note string) string {
	return `<!doctype html><html lang="zh-Hant"><head><meta charset="utf-8">` +
		`<title>qooq</title><style>` +
		`body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;` +
		`background:#15171a;color:#e8eaed;font:16px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}` +
		`div{text-align:center}h1{font-size:20px;margin:0 0 6px}p{color:#9aa0a6;font-size:14px;margin:0}` +
		`</style></head><body><div><h1>` + title + `</h1><p>` + note + `</p></div></body></html>`
}

// controlKey is how one qooq Cloud server is identified in the token store.
//
// Deliberately not the address the CLI dials. That address stays exactly as it
// was typed, because somebody writing localhost may mean an /etc/hosts entry or
// the name on a certificate, and rewriting it would change which server is
// contacted. The key answers a different question -- *which* qooq Cloud server is
// this -- and there http://localhost:8080 and http://127.0.0.1:8080 are one
// answer. Keying on the spelling is what let a successful sign-in sit next to
// "the stored token is not accepted": two entries, one machine.
//
// Narrow on purpose: scheme and host case, the default port, a trailing slash,
// and the loopback spellings. Merging two qooq Cloud servers' credentials is a
// worse failure than leaving one machine's spellings apart, so anything short
// of certain stays apart -- including names under .localhost, which can be two
// different servers told apart by the Host header.
func controlKey(addr string) string {
	raw := strings.TrimRight(addr, "/")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()

	if ip := net.ParseIP(host); (ip != nil && ip.IsLoopback()) || host == "localhost" {
		host = "localhost"
	}
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	switch {
	case port != "":
		host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		host = "[" + host + "]" // a bare IPv6 literal
	}
	// The path is kept, and kept case sensitive: a qooq Cloud server can live under
	// a prefix behind a shared proxy.
	return scheme + "://" + host + strings.TrimRight(u.Path, "/")
}
