package tape

import (
	"testing"
	"time"
)

func TestSimulatorPermitSingleUseScopeAndExpiry(t *testing.T) {
	now := time.Now()
	scope := SimulatorScope{Observation: Observation{Drive: "simulator:test", Nonce: "nonce", Lease: "lease", MediaGeneration: "generation"}, FileIndex: 1, IntentID: "intent"}
	p := NewSimulatorPermit(scope, now.Add(time.Minute))
	if err := ConsumeSimulatorPermit(p, scope, now); err != nil {
		t.Fatal(err)
	}
	if err := ConsumeSimulatorPermit(p, scope, now); err == nil {
		t.Fatal("permit replay")
	}
	p = NewSimulatorPermit(scope, now)
	if err := ConsumeSimulatorPermit(p, scope, now); err == nil {
		t.Fatal("expired permit")
	}
	p = NewSimulatorPermit(scope, now.Add(time.Minute))
	changed := scope
	changed.Observation.Nonce = "other"
	if err := ConsumeSimulatorPermit(p, changed, now); err == nil {
		t.Fatal("changed identity")
	}
	if err := ConsumeSimulatorPermit(p, scope, now); err == nil {
		t.Fatal("rejected permit reused")
	}
}
