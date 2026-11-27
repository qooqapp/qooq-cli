package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/qooqapp/qooq/pkg/watcherclient"
)

// A target in needs_input with no missing inputs is waiting for another
// component. It used to print "no answer yet ()" and point at set-input.
func TestFinishLocalNeedsInputWaitingOnComponent(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := finishLocal(cmd, nil, &watcherclient.TargetResponse{
		Name:            "web",
		LastBuildStatus: statusNeedsInput,
		LastBuildError:  `component "api" is not deployed yet`,
	}, "")
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if strings.Contains(msg, "()") || strings.Contains(msg, "set-input") {
		t.Errorf("waiting message still reads as a missing input: %q", msg)
	}
	if !strings.Contains(msg, `component "api" is not deployed yet`) {
		t.Errorf("waiting message does not say what it waits for: %q", msg)
	}
}

func TestFinishLocalNeedsInputMissingValue(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := finishLocal(cmd, nil, &watcherclient.TargetResponse{
		Name:            "web",
		LastBuildStatus: statusNeedsInput,
		MissingInputs:   []string{"STRIPE_KEY", "OTHER"},
	}, "")
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{"(STRIPE_KEY, OTHER)", "qooq watch set-input web STRIPE_KEY=<value>"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in %q", want, msg)
		}
	}
}
