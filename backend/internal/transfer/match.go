// Package transfer joins the two halves of a movement between venues.
//
// K49 settled the intra-venue case: Binance reports one row naming both wallets, so there is
// nothing to match. This package is the case K12 actually described -- coins leaving one
// integration and arriving at another, as two independent ledger events that no venue will
// ever tell us are the same movement.
//
// It matters because an unmatched pair is read as a disposal plus an acquisition: PnL
// collapses, and the user sees a realized loss they never took. It is the industry's
// number-one support topic, and it is a matching problem with no authoritative answer -- which
// is why the rules below refuse more often than they guess.
//
// Match is pure (L4). The legs and the rules are inputs; nothing here reads a clock.
package transfer

import (
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Leg is one half of a possible transfer: a withdrawal, or a deposit.
type Leg struct {
	// EventID is the ledger event this leg is, as a string, because that is what a link
	// stores and what a user resolving the queue points at.
	EventID       string
	IntegrationID uuid.UUID
	Venue         string

	Asset    string
	Quantity decimal.Decimal

	// TxID is the chain transaction, when the venue reported one. Empty is common and is not
	// an error: an internal transfer between two exchange accounts never touches a chain.
	TxID string

	EventTime time.Time
}

// Rules are the heuristic's two dials.
type Rules struct {
	// Window is how long after a withdrawal its deposit may arrive. One-directional on
	// purpose: coins do not arrive before they leave, and a symmetric window would happily
	// say they did.
	Window time.Duration

	// FeeTolerance is how much smaller the deposit may be, as a fraction of the withdrawal.
	// The shortfall is the network fee. A deposit LARGER than its withdrawal is never a
	// match -- money does not appear in transit.
	FeeTolerance decimal.Decimal
}

// Link is one matched movement, and how it was decided.
type Link struct {
	Out, In Leg

	// Method is "txid" or "heuristic". Stored because a link decided by proof and one decided
	// by a guess are different claims, and a user resolving a dispute has to be able to tell
	// them apart.
	Method string
}

// Method values.
const (
	MethodTxID      = "txid"
	MethodHeuristic = "heuristic"
)

// Result is every link the rules could justify, and everything they could not.
type Result struct {
	Links []Link

	// UnmatchedOut and UnmatchedIn are the queue. They are not failures: a deposit whose
	// withdrawal was never ingestible has no match to find (B2, F5), and saying so is the
	// only honest answer.
	UnmatchedOut []Leg
	UnmatchedIn  []Leg
}

// Match pairs withdrawals with deposits.
//
// Two passes, and the order between them is the decision (K57): a txid match is proof and is
// taken first; only then is the amount-and-window heuristic allowed to look at what is left.
// Running them together would let a closer guess outrank a chain transaction that two venues
// independently agreed on.
func Match(outs, ins []Leg, rules Rules) Result {
	outs, ins = sorted(outs), sorted(ins)
	claimedOut := make(map[string]bool, len(outs))
	claimedIn := make(map[string]bool, len(ins))

	var links []Link

	// Pass one: proof.
	for _, o := range outs {
		if o.TxID == "" {
			continue
		}
		for _, i := range ins {
			if claimedIn[i.EventID] || i.TxID != o.TxID || !plausible(o, i, rules) {
				continue
			}
			links = append(links, Link{Out: o, In: i, Method: MethodTxID})
			claimedOut[o.EventID], claimedIn[i.EventID] = true, true
			break
		}
	}

	// Pass two: the heuristic, over what proof did not claim.
	for _, o := range outs {
		if claimedOut[o.EventID] {
			continue
		}
		candidates := make([]Leg, 0, 2)
		for _, i := range ins {
			if claimedIn[i.EventID] || !plausible(o, i, rules) {
				continue
			}
			candidates = append(candidates, i)
		}
		// Ambiguity is refused, not broken by time. Choosing the nearer candidate is right
		// most of the time, and the times it is wrong look exactly like the times it is right
		// (K57).
		if len(candidates) != 1 {
			continue
		}
		links = append(links, Link{Out: o, In: candidates[0], Method: MethodHeuristic})
		claimedOut[o.EventID], claimedIn[candidates[0].EventID] = true, true
	}

	sort.Slice(links, func(a, b int) bool { return links[a].Out.EventID < links[b].Out.EventID })

	return Result{
		Links:        links,
		UnmatchedOut: leftover(outs, claimedOut),
		UnmatchedIn:  leftover(ins, claimedIn),
	}
}

// plausible is everything both passes require, so proof cannot skip a check the guess makes.
// A txid that agreed while the asset did not would be a coincidence of hashes, not a transfer.
func plausible(o, i Leg, rules Rules) bool {
	if o.Asset != i.Asset {
		return false
	}
	// The same integration is K49's case: one row that names both wallets, already folded.
	// Matching it here would count the movement twice.
	if o.IntegrationID == i.IntegrationID {
		return false
	}
	if i.EventTime.Before(o.EventTime) {
		return false
	}
	if i.EventTime.Sub(o.EventTime) > rules.Window {
		return false
	}
	// Larger is never a match; smaller is a match only within the fee tolerance.
	if i.Quantity.GreaterThan(o.Quantity) {
		return false
	}
	shortfall := o.Quantity.Sub(i.Quantity)
	return shortfall.LessThanOrEqual(o.Quantity.Mul(rules.FeeTolerance))
}

// sorted makes the result reproducible. Ranging in arrival order would let a re-run rewrite a
// queue the user is part-way through resolving.
func sorted(legs []Leg) []Leg {
	out := append([]Leg(nil), legs...)
	sort.Slice(out, func(a, b int) bool {
		if !out[a].EventTime.Equal(out[b].EventTime) {
			return out[a].EventTime.Before(out[b].EventTime)
		}
		return out[a].EventID < out[b].EventID
	})
	return out
}

func leftover(legs []Leg, claimed map[string]bool) []Leg {
	out := make([]Leg, 0, len(legs))
	for _, l := range legs {
		if !claimed[l.EventID] {
			out = append(out, l)
		}
	}
	return out
}
