package audit

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveCLIActor_HumanOK(t *testing.T) {
	actor, err := ResolveCLIActor("farshid", "human", "", []string{"-actor-id=farshid"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if actor.Type != ActorHuman || actor.ID != "farshid" {
		t.Fatalf("unexpected actor: %+v", actor)
	}
	if actor.Input != "" {
		t.Fatalf("expected no Input for a human actor, got %q", actor.Input)
	}
}

func TestResolveCLIActor_AIAgentOK_SetsInputFromArgs(t *testing.T) {
	args := []string{"-actor-id=pipeline", "-actor-type=ai_agent", "-model-version=claude-fable-5"}
	actor, err := ResolveCLIActor("pipeline", "ai_agent", "claude-fable-5", args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if actor.Type != ActorAgent || actor.ModelVersion != "claude-fable-5" {
		t.Fatalf("unexpected actor: %+v", actor)
	}
	want := CLIInvocationInput(args)
	if actor.Input != want {
		t.Fatalf("expected Input to be CLIInvocationInput(args), got %q want %q", actor.Input, want)
	}
}

func TestResolveCLIActor_UnknownActorType_Rejected(t *testing.T) {
	_, err := ResolveCLIActor("pipeline", "wizard", "", nil)
	if err == nil || !strings.Contains(err.Error(), "-actor-type must be") {
		t.Fatalf("expected an unknown actor type to be rejected, got %v", err)
	}
}

// The half of the falsification Validate() alone doesn't catch: a human
// actor carrying a model version (uc-infra#72 independent review, finding
// 4; the bug this helper's own extraction (uc-infra#167) exists to stop
// from silently going missing on any one of its ten callers again, the
// way it already had on cmd/install-module before this change).
func TestResolveCLIActor_HumanWithModelVersion_Rejected(t *testing.T) {
	_, err := ResolveCLIActor("pipeline", "human", "claude-fable-5", nil)
	if err == nil || !strings.Contains(err.Error(), "-model-version is only meaningful") {
		t.Fatalf("expected a human actor with -model-version set to be rejected, got %v", err)
	}
}

func TestResolveCLIActor_AIAgentMissingModelVersion_Rejected(t *testing.T) {
	_, err := ResolveCLIActor("pipeline", "ai_agent", "", nil)
	if !errors.Is(err, ErrMissingModelVersion) {
		t.Fatalf("expected ErrMissingModelVersion, got %v", err)
	}
}

func TestResolveCLIActor_MissingActorID_Rejected(t *testing.T) {
	_, err := ResolveCLIActor("", "human", "", nil)
	if !errors.Is(err, ErrMissingActorID) {
		t.Fatalf("expected ErrMissingActorID, got %v", err)
	}
}

// A rejected human-with-model-version request never falls through to
// Validate() with a half-built actor — confirms the check order rather
// than assuming it from the code above.
func TestResolveCLIActor_RejectedActor_IsZeroValue(t *testing.T) {
	actor, err := ResolveCLIActor("pipeline", "human", "claude-fable-5", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if actor != (Actor{}) {
		t.Fatalf("expected a zero-value Actor on error, got %+v", actor)
	}
}
