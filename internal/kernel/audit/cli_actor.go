package audit

import "fmt"

// ResolveCLIActor builds and validates the audit Actor for a cmd/ binary
// invocation from its standard -actor-id/-actor-type/-model-version flags.
//
// Extracted from 10 near-identical ~20-line copies across cmd/* (uc-infra#167)
// that had already drifted once (cmd/install-module was missing the
// human-carrying-a-model-version guard every other copy had). The logic
// itself predates the extraction — see uc-infra#72/#123/#124/#161, whose
// reasoning is preserved in the comments below rather than restated fresh:
//
//   - An unattended pipeline run is an ai_agent actor, and ADR-0001 §14
//     makes that distinction first-class — hard-coding ActorHuman would
//     write a falsified actor_type onto every row the command touches
//     (uc-infra#72/#123).
//   - An ai_agent actor has no natural-language prompt of its own here —
//     the resolved invocation is the closest analogue, and ADR-0001 §14
//     requires input_hash for every ai_agent audit row, not just
//     model_version (uc-infra#124, uc-infra#161).
//   - A human actor carrying a model version is the same class of
//     falsified audit metadata the ai_agent-without-ModelVersion check
//     exists to prevent, the other way around (uc-infra#72 independent
//     review, finding 4) — Validate() alone only rejects an EMPTY
//     ModelVersion on an agent, never a populated one on a human, so that
//     half of the mistake needs its own check.
//
// args is the invocation's argument list for CLIInvocationInput — callers
// pass os.Args[1:] (excluding the program name, which varies by how the
// binary was invoked and carries no information about what it did).
//
// Validation runs, and this returns an error, before any database work in
// every caller: an operator who mistyped the actor should learn that
// immediately, not after a connection or a tenant lookup already ran.
// Callers that want the historical "invalid actor: ..." wording on exit
// do so themselves (log.Fatalf("invalid actor: %v", err)) — this function
// returns the unprefixed error so a caller that wants different framing
// isn't stuck with one baked in.
func ResolveCLIActor(actorID, actorType, modelVersion string, args []string) (Actor, error) {
	actor := Actor{Type: ActorType(actorType), ID: actorID, ModelVersion: modelVersion}
	switch actor.Type {
	case ActorHuman, ActorAgent:
	default:
		return Actor{}, fmt.Errorf("-actor-type must be %q or %q, got %q", ActorHuman, ActorAgent, actorType)
	}
	if actor.Type == ActorAgent {
		actor.Input = CLIInvocationInput(args)
	}
	if actor.Type == ActorHuman && modelVersion != "" {
		return Actor{}, fmt.Errorf("-model-version is only meaningful when -actor-type is %q", ActorAgent)
	}
	if err := actor.Validate(); err != nil {
		return Actor{}, err
	}
	return actor, nil
}
