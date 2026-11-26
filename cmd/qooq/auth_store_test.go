package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests are about one thing: a qooq Cloud server has more than one spelling,
// and a credential belongs to the machine, not to the spelling somebody typed.
//
// The bug they come from: signing in with `--control-addr http://localhost:8080`
// stored a token under that string, while every later command defaulted to
// http://127.0.0.1:8080 and found a different, older entry. It then reported
// "the stored token is not accepted (expired or revoked)" — true of the token it
// looked at, and badly misleading to somebody who had just signed in
// successfully.

func writeRawStore(t *testing.T, raw string) {
	t.Helper()
	t.Setenv("QOOQ_HOME", t.TempDir())
	p, err := authPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOneControlPlaneSpelledTwoWaysIsOneLogin(t *testing.T) {
	t.Setenv("QOOQ_HOME", t.TempDir())
	if err := saveLogin("http://localhost:8080", storedLogin{Token: "tok", Account: "acme"}); err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{
		"http://127.0.0.1:8080",
		"http://localhost:8080",
		"http://LocalHost:8080",
		"http://[::1]:8080",
		"http://127.0.0.1:8080/",
	} {
		l, ok, err := loadLogin(spelling)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || l.Token != "tok" {
			t.Errorf("%s: signed in at localhost:8080 but this spelling found nothing (ok=%v token=%q)", spelling, ok, l.Token)
		}
	}
}

// The file this session actually found had both spellings in it, so the lookup
// has to be decided rather than discovered: a map range would hand back
// whichever Go felt like, which is a credential that works on some runs.
func TestTheNewestLoginWinsWhenBothSpellingsAreStored(t *testing.T) {
	writeRawStore(t, `{"logins":{
	  "http://127.0.0.1:8080": {"token":"old","account":"alice","signed_in_at":"2026-10-01T09:27:15Z"},
	  "http://localhost:8080": {"token":"new","account":"alice","signed_in_at":"2026-10-29T03:35:14Z"}
	}}`)
	for i := 0; i < 200; i++ {
		l, ok, err := loadLogin("http://127.0.0.1:8080")
		if err != nil {
			t.Fatal(err)
		}
		if !ok || l.Token != "new" {
			t.Fatalf("attempt %d returned %q: the most recent sign-in has to win every time, not most of the time", i, l.Token)
		}
	}
}

func TestSigningInDropsTheOtherSpelling(t *testing.T) {
	writeRawStore(t, `{"logins":{
	  "http://127.0.0.1:8080": {"token":"old","account":"alice","signed_in_at":"2026-10-01T09:27:15Z"}
	}}`)
	if err := saveLogin("http://localhost:8080", storedLogin{
		Token: "new", Account: "alice", SignedInAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	store, err := loadStore()
	if err != nil {
		t.Fatal(err)
	}
	if len(store.Logins) != 1 {
		keys, _ := json.Marshal(store.Logins)
		t.Fatalf("signing in left %d entries for one qooq Cloud server: %s", len(store.Logins), keys)
	}
	for _, l := range store.Logins {
		if l.Token != "new" {
			t.Errorf("the surviving entry is the old one (%q)", l.Token)
		}
	}
}

func TestLogoutForgetsEverySpelling(t *testing.T) {
	writeRawStore(t, `{"logins":{
	  "http://127.0.0.1:8080": {"token":"old","account":"alice","signed_in_at":"2026-10-01T09:27:15Z"},
	  "http://localhost:8080": {"token":"new","account":"alice","signed_in_at":"2026-10-29T03:35:14Z"}
	}}`)
	if err := authLogout("http://localhost:8080"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := loadLogin("http://127.0.0.1:8080"); ok {
		t.Error("logout left a credential behind under the other spelling, so the next command signs in as somebody again")
	}
}

// The other direction, and the reason this normalisation is narrow: merging two
// qooq Cloud servers' credentials is worse than failing to merge one's spellings.
func TestDifferentControlPlanesStayApart(t *testing.T) {
	for _, pair := range [][2]string{
		{"http://localhost:8080", "http://localhost:9090"},
		{"http://localhost:8080", "https://localhost:8080"},
		{"http://localhost:8080", "http://control.example.com:8080"},
		{"http://localhost:8080", "http://localhost:8080/other"},
		{"http://10.0.0.1:8080", "http://10.0.0.2:8080"},
		// Not collapsed on purpose: two names under .localhost can be two
		// different servers told apart by the Host header.
		{"http://localhost:8080", "http://dev.localhost:8080"},
	} {
		t.Setenv("QOOQ_HOME", t.TempDir())
		if err := saveLogin(pair[0], storedLogin{Token: "a", Account: "acme"}); err != nil {
			t.Fatal(err)
		}
		if l, ok, _ := loadLogin(pair[1]); ok {
			t.Errorf("%s and %s are different qooq Cloud servers but share a credential (%q)", pair[0], pair[1], l.Token)
		}
	}
}

func TestControlKeyCanonicalForm(t *testing.T) {
	same := [][2]string{
		{"http://localhost:8080", "http://127.0.0.1:8080"},
		{"http://localhost:8080", "http://[::1]:8080"},
		{"http://localhost:8080", "http://127.0.0.2:8080"},
		{"http://localhost:8080/", "http://localhost:8080"},
		{"HTTP://LOCALHOST:8080", "http://localhost:8080"},
		{"http://localhost", "http://localhost:80"},
		{"https://example.com", "https://example.com:443"},
		{"https://Example.COM/base/", "https://example.com/base"},
	}
	for _, p := range same {
		if controlKey(p[0]) != controlKey(p[1]) {
			t.Errorf("%s and %s name the same qooq Cloud server but key to %q and %q",
				p[0], p[1], controlKey(p[0]), controlKey(p[1]))
		}
	}
}
