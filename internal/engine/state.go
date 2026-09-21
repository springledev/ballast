// Package engine implements the Ballast job execution engine: the
// per-run state machine (spec §4.1) and the runner that drives a job
// through it.
//
// Every backup run MUST transition through exactly this state machine,
// persisted in the DB:
//
//	PENDING → PREPARING → DUMPING → UPLOADING → VERIFYING → SUCCESS
//
// with FAILED reachable from any non-terminal state and CANCELLED
// reachable from any state. VERIFYING is mandatory: a run that never
// reaches VERIFYING cannot be marked SUCCESS; if verification is
// impossible the run ends as SUCCESS_UNVERIFIED (amber), never green.
package engine

import "fmt"

// State is a state in the job state machine.
type State string

// All states of a job run, in pipeline order plus terminal states.
const (
	PENDING            State = "PENDING"
	PREPARING          State = "PREPARING"
	DUMPING            State = "DUMPING"
	UPLOADING          State = "UPLOADING"
	VERIFYING          State = "VERIFYING"
	SUCCESS            State = "SUCCESS"
	SUCCESS_UNVERIFIED State = "SUCCESS_UNVERIFIED"
	FAILED             State = "FAILED"
	CANCELLED          State = "CANCELLED"
)

var allStates = []State{
	PENDING, PREPARING, DUMPING, UPLOADING, VERIFYING,
	SUCCESS, SUCCESS_UNVERIFIED, FAILED, CANCELLED,
}

// AllStates returns every defined state, for API/status surfaces.
func AllStates() []State { return append([]State(nil), allStates...) }

// valid reports whether s is a defined state.
func valid(s State) bool {
	for _, k := range allStates {
		if k == s {
			return true
		}
	}
	return false
}

// terminalStates are states from which no further transition is
// possible. A run in a terminal state is finished.
var terminalStates = map[State]bool{
	SUCCESS:            true,
	SUCCESS_UNVERIFIED: true,
	FAILED:             true,
	CANCELLED:          true,
}

// Terminal reports whether s is a terminal state: no further legal
// transitions exist from it. A run that reaches FAILURE or
// CANCELLATION is over; a retry always starts a new run.
func (s State) Terminal() bool { return terminalStates[s] }

// legalTransitions is the adjacency table of the state machine
// (spec §4.1). It is the single source of truth for what may move to
// what; everything else derives from it.
var legalTransitions = map[State][]State{
	PENDING: {PREPARING, FAILED, CANCELLED},
	// PREPARING does dump/up work: upload comes after dump is complete.
	PREPARING: {DUMPING, FAILED, CANCELLED},
	DUMPING:   {UPLOADING, FAILED, CANCELLED},
	UPLOADING: {VERIFYING, FAILED, CANCELLED},
	// VERIFYING is mandatory before SUCCESS (spec §4.1) — SUCCESS is
	// not listed as legal from any other state. If verification is
	// impossible the run is amber (SUCCESS_UNVERIFIED), never green.
	VERIFYING: {SUCCESS, SUCCESS_UNVERIFIED, FAILED, CANCELLED},
}

// CanTransition reports whether from → to is a legal state machine
// transition per spec §4.1.
func CanTransition(from, to State) bool {
	if !valid(from) || !valid(to) {
		return false
	}
	if from.Terminal() {
		return false
	}
	for _, allowed := range legalTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// NextStates returns the legal successor states of from.
func NextStates(from State) []State {
	if !valid(from) || from.Terminal() {
		return nil
	}
	return append([]State(nil), legalTransitions[from]...)
}

func (s State) String() string {
	if !valid(s) {
		return fmt.Sprintf("INVALID(%s)", string(s))
	}
	return string(s)
}

// Amber reports whether the state represents the "backup made but not
// green" condition: SUCCESS_UNVERIFIED per spec §4.1.
func (s State) Amber() bool { return s == SUCCESS_UNVERIFIED }
