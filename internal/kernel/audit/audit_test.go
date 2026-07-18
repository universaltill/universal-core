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
			Actor{Type: ActorAgent, ID: "kernel-agent", ModelVersion: "claude-fable-5"},
			nil,
		},
		{"missing id", Actor{Type: ActorHuman, ID: ""}, ErrMissingActorID},
		{
			"ai agent missing model version",
			Actor{Type: ActorAgent, ID: "kernel-agent"},
			ErrMissingModelVersion,
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
	_, err := New("tenant-1", "Vendor", "rec-1", ActionCreate, Actor{Type: ActorAgent, ID: "a"}, nil)
	if err != ErrMissingModelVersion {
		t.Fatalf("expected ErrMissingModelVersion, got %v", err)
	}
}

func TestNew_Succeeds(t *testing.T) {
	e, err := New("tenant-1", "Vendor", "rec-1", ActionCreate,
		Actor{Type: ActorHuman, ID: "farshid"}, map[string]any{"name": "Acme"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.TenantID != "tenant-1" || e.Action != ActionCreate {
		t.Fatalf("unexpected entry: %+v", e)
	}
}
