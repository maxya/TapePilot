package tape

import (
	"errors"
	"sync"
	"time"
)

// SimulatorScope identifies one simulated writer opening. Physical adapters must
// never accept these permits. The simulator also checks its durable media state.
type SimulatorScope struct {
	Observation Observation
	FileIndex   uint64
	IntentID    string
}

type simulatorPermit struct {
	mu      sync.Mutex
	scope   SimulatorScope
	expires time.Time
	used    bool
}

func (*simulatorPermit) writePermit() {}

// NewSimulatorPermit is available only inside the backend's internal package
// boundary. An engine must persist its intent and validate the archive chain
// before asking its simulator lease to authorize an opening.
func NewSimulatorPermit(scope SimulatorScope, expires time.Time) WritePermit {
	return &simulatorPermit{scope: scope, expires: expires}
}

// ConsumeSimulatorPermit atomically consumes matching simulated authority.
// A rejected or already used permit can never authorize a later writer opening.
func ConsumeSimulatorPermit(permit WritePermit, scope SimulatorScope, now time.Time) error {
	p, ok := permit.(*simulatorPermit)
	if !ok || p == nil {
		return errors.New("WRITE_NOT_ARMED")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used {
		return errors.New("WRITE_NOT_ARMED")
	}
	p.used = true
	if p.scope != scope || scope.IntentID == "" || !now.Before(p.expires) {
		return errors.New("WRITE_NOT_ARMED")
	}
	return nil
}
