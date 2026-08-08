package audit

import "testing"

func TestActorValidate(t *testing.T) {
	cases := []struct {
		name    string
		actor   Actor
		wantErr error
	}{
		{"human ok", Actor{Type: ActorHuman, ID: "farshid"}, nil},
		{
			"ai agent ok",
			Actor{Type: ActorAgent, ID: "kernel-agent", ModelVersion: "claude-fable-5", Input: "add a field"},
			nil,
		},
		{"missing id", Actor{Type: ActorHuman, ID: ""}, ErrMissingActorID},
		{
			"ai agent missing model version",
			Actor{Type: ActorAgent, ID: "kernel-agent", Input: "add a field"},
			ErrMissingModelVersion,
		},
		{
			// uc-infra#161: every real ActorAgent call site now populates
			// Input (the CLI binaries directly; entity's production
			// ActorAgent callers are those same CLI binaries via
			// moduleseed; csvimport/sqlsource have no production
			// ActorAgent caller at all), so this is a hard reject, not the
			// "still valid" carve-out uc-infra#124 left in place.
			"ai agent missing input",
			Actor{Type: ActorAgent, ID: "kernel-agent", ModelVersion: "claude-fable-5"},
			ErrMissingInput,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.actor.Validate()
			if err != tc.wantErr {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestInputHash(t *testing.T) {
	a := Actor{Type: ActorAgent, ID: "x", ModelVersion: "v1", Input: "add a field"}
	h1 := a.InputHash()
	if h1 == "" {
		t.Fatal("expected non-empty hash for non-empty input")
	}
	a2 := Actor{Type: ActorAgent, ID: "x", ModelVersion: "v1", Input: "add a field"}
	if a2.InputHash() != h1 {
		t.Fatal("expected identical input to hash identically")
	}
	a3 := Actor{Type: ActorAgent, ID: "x", ModelVersion: "v1", Input: "add a different field"}
	if a3.InputHash() == h1 {
		t.Fatal("expected different input to hash differently")
	}
	empty := Actor{Type: ActorHuman, ID: "x"}
	if empty.InputHash() != "" {
		t.Fatal("expected empty hash for empty input")
	}
}

func TestNew_RejectsInvalidActor(t *testing.T) {
	_, err := New("Vendor", "rec-1", ActionCreate, Actor{Type: ActorAgent, ID: "a"}, nil)
	if err != ErrMissingModelVersion {
		t.Fatalf("expected ErrMissingModelVersion, got %v", err)
	}
}

func TestNew_RejectsMissingInput(t *testing.T) {
	_, err := New("Vendor", "rec-1", ActionCreate,
		Actor{Type: ActorAgent, ID: "a", ModelVersion: "v1"}, nil)
	if err != ErrMissingInput {
		t.Fatalf("expected ErrMissingInput, got %v", err)
	}
}

func TestNew_Succeeds(t *testing.T) {
	e, err := New("Vendor", "rec-1", ActionCreate,
		Actor{Type: ActorHuman, ID: "farshid"}, map[string]any{"name": "Acme"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.EntityType != "Vendor" || e.Action != ActionCreate {
		t.Fatalf("unexpected entry: %+v", e)
	}
}

func TestCLIInvocationInput(t *testing.T) {
	a := CLIInvocationInput([]string{"-actor-type=ai_agent", "-model-version=claude-fable-5", "-tenant-id=t1"})
	b := CLIInvocationInput([]string{"-actor-type=ai_agent", "-model-version=claude-fable-5", "-tenant-id=t1"})
	if a == "" {
		t.Fatal("expected non-empty input for a non-empty argument list")
	}
	if a != b {
		t.Fatalf("expected the same argument list to produce the same input, got %q vs %q", a, b)
	}

	c := CLIInvocationInput([]string{"-actor-type=ai_agent", "-model-version=claude-fable-5", "-tenant-id=t2"})
	if c == a {
		t.Fatal("expected a different argument list to produce a different input")
	}

	if got := CLIInvocationInput(nil); got != "" {
		t.Fatalf("expected empty input for no arguments, got %q", got)
	}

	// A plain space-join would hash these two differently-shaped
	// invocations identically (independent review, uc-infra#124) —
	// quoting each argument must keep them distinguishable.
	merged := CLIInvocationInput([]string{"-name", "a b"})
	split := CLIInvocationInput([]string{"-name", "a", "b"})
	if merged == split {
		t.Fatalf("expected a single two-word argument to hash differently than two separate arguments, got identical input %q", merged)
	}
}
