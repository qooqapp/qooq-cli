package main

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestLocalConfigDefaultsAndRoundTrip(t *testing.T) {
	home := t.TempDir()
	c, err := loadLocalConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	if c.ingressPort() != 80 || c.access() != "lan" {
		t.Fatalf("defaults: port %d access %q, want 80 and lan", c.ingressPort(), c.access())
	}
	p := 8080
	c.IngressPort = &p
	c.Access = "local"
	if err := saveLocalConfig(home, c); err != nil {
		t.Fatal(err)
	}
	back, err := loadLocalConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	if back.ingressPort() != 8080 || back.access() != "local" {
		t.Fatalf("round trip: port %d access %q", back.ingressPort(), back.access())
	}

	off := 0
	c.IngressPort = &off
	if err := saveLocalConfig(home, c); err != nil {
		t.Fatal(err)
	}
	if back, _ := loadLocalConfig(home); back.ingressPort() != 0 {
		t.Fatalf("0 must stay 0 (ingress off), got %d", back.ingressPort())
	}
}

func TestLocalConfigRejectsBadValues(t *testing.T) {
	home := t.TempDir()
	bad := 70000
	if err := saveLocalConfig(home, localConfig{IngressPort: &bad}); err == nil {
		t.Fatal("port 70000 was accepted")
	}
	if err := saveLocalConfig(home, localConfig{Access: "public"}); err == nil || !strings.Contains(err.Error(), "local") {
		t.Fatalf("err = %v, want it to name the valid choices", err)
	}
}

func TestQooqdArgsFromSettings(t *testing.T) {
	lan := localConfig{}
	if got := strings.Join(lan.qooqdArgs(80), " "); got != "-ingress -ingress-http-port 80" {
		t.Fatalf("lan, port 80: %q", got)
	}
	if got := lan.qooqdArgs(0); len(got) != 0 {
		t.Fatalf("lan, ingress off: %q, want no flags", got)
	}
	local := localConfig{Access: "local"}
	got := strings.Join(local.qooqdArgs(8080), " ")
	for _, want := range []string{"-ingress-http-port 8080", "-publish-ip 127.0.0.1", "-ingress-bind-ip 127.0.0.1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("local, port 8080: %q lacks %q", got, want)
		}
	}
}

func TestIngressPortBusy(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if !ingressPortBusy(localConfig{Access: "local"}, port) {
		t.Fatalf("port %d is held but was reported free", port)
	}
	ln.Close()
	if ingressPortBusy(localConfig{Access: "local"}, port) {
		t.Fatalf("port %s is free but was reported busy", strconv.Itoa(port))
	}
}
