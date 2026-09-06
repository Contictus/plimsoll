package worker

import (
	"github.com/Contictus/plimsoll/backend/internal/httpapi"
)

// State is what the ingestion for one integration is currently doing. It exists so that a
// response can say what it is built on: every state but one costs the reader something, and
// L11 says that cost is reported rather than hidden.
type State string

// The states an integration's ingestion can be in.
const (
	// StateConnecting is before the first successful subscribe. Nothing live is arriving
	// yet, and it is not yet known whether anything will.
	StateConnecting State = "connecting"

	// StateLive is the only clean state: subscribed, connected, and history complete.
	StateLive State = "live"

	// StateDegraded is subscribed but not connected. The feed is down and the account may
	// be trading; this is the worst thing that can be true without the worker having
	// stopped.
	StateDegraded State = "degraded"

	// StateResyncing is replaying a window the stream was disconnected for. Live again, but
	// not yet caught up.
	StateResyncing State = "resyncing"

	// StateBackfilling is live, with history still loading behind it.
	StateBackfilling State = "backfilling"
)

// AllStates is every state, and it is what the freshness test walks. A state added to the
// constants above but not here is a state no test checks, so keep them together.
var AllStates = []State{
	StateConnecting,
	StateLive,
	StateDegraded,
	StateResyncing,
	StateBackfilling,
}

// stateReasons is the whole of what a state costs a reader. The map is the contract the
// test enforces: every state has an entry, and only StateLive's is absent.
//
// The codes come from httpapi rather than being spelled again here. A second spelling of a
// reason code is a response no client can match on, and the constants exist precisely to
// make that impossible -- which is worth this package depending on the HTTP one for a
// vocabulary both of them speak.
var stateReasons = map[State]httpapi.Reason{
	StateConnecting: {
		Code:     httpapi.ReasonWSGap,
		Severity: httpapi.SeverityWarn,
		Detail:   "the live feed has not connected yet; events since the last run may be missing",
	},
	StateDegraded: {
		Code:     httpapi.ReasonWSGap,
		Severity: httpapi.SeverityError,
		Detail:   "the live feed is down; trades happening now are not being recorded",
	},
	StateResyncing: {
		Code:     httpapi.ReasonWSGap,
		Severity: httpapi.SeverityWarn,
		Detail:   "replaying the window the live feed was disconnected for",
	},
	StateBackfilling: {
		Code:     httpapi.ReasonBackfillIncomplete,
		Severity: httpapi.SeverityWarn,
		Detail:   "historical import is still running; totals do not yet cover the full history",
	},
}

// Reason returns what this state contributes to a response's freshness, and whether it
// contributes anything at all. Only StateLive returns false.
//
// Since is deliberately not set here: the state knows what is wrong, not how long it has
// been. The supervisor stamps it from the moment it entered the state.
func (s State) Reason() (httpapi.Reason, bool) {
	reason, degraded := stateReasons[s]
	return reason, degraded
}

// Conditions is what the supervisor currently knows. Classify turns it into one state, so
// the precedence lives in one place rather than being re-decided at each call site.
type Conditions struct {
	// Subscribed is whether a subscription has ever been established. Before that there is
	// nothing to be degraded from.
	Subscribed bool
	// Connected is whether the stream is delivering right now.
	Connected bool
	// Resyncing is whether a gap window is being replayed.
	Resyncing bool
	// HistoryComplete is whether every backfill scope has finished.
	HistoryComplete bool
}

// Classify picks the one state that describes these conditions, worst first.
//
// The order is the reader's order, not the worker's. A worker that is both backfilling and
// disconnected is reported as disconnected, because "the live feed is down" changes what a
// reader should do with the number in front of them and "history is still loading" mostly
// does not.
func Classify(c Conditions) State {
	switch {
	case !c.Subscribed:
		return StateConnecting
	case !c.Connected:
		return StateDegraded
	case c.Resyncing:
		return StateResyncing
	case !c.HistoryComplete:
		return StateBackfilling
	default:
		return StateLive
	}
}
