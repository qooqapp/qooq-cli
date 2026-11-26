package main

import (
	"strings"
	"testing"
)

func TestRefusalNamesTheConsoleAndTheNextStep(t *testing.T) {
	err := refusalError("access_code_required", "http://example.test:8080/")
	if err == nil || !strings.Contains(err.Error(), "http://example.test:8080/") ||
		!strings.Contains(err.Error(), "qooq auth login") {
		t.Fatalf("refusalError = %v, want the console address and what to run next", err)
	}
	_, note := refusalPage("access_code_required", "http://example.test:8080")
	if !strings.Contains(note, "http://example.test:8080/") {
		t.Fatalf("refusal page does not link the console: %s", note)
	}
}

// The identifier arrives in a redirect anyone can forge, so it must not reach
// the page and must not write control characters to the terminal.
func TestAForgedRefusalIsNotEchoed(t *testing.T) {
	forged := "<script>x</script>\x1b[2J"
	title, note := refusalPage(forged, "http://example.test")
	if strings.Contains(title+note, "<script>") {
		t.Fatalf("forged identifier reached the page: %s %s", title, note)
	}
	if msg := refusalError(forged, "http://example.test").Error(); strings.ContainsAny(msg, "<>\x1b") {
		t.Fatalf("forged identifier reached the terminal: %q", msg)
	}
}
