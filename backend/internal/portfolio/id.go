package portfolio

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// ErrMalformedID means the string is not a position id. Returned rather than a zero value,
// so a handler answers 400 instead of looking up a position that could never exist.
var ErrMalformedID = errors.New("portfolio: malformed position id")

// idSeparator is a dot because neither half can contain one: a UUID is hex and hyphens, an
// instrument id is digits.
const idSeparator = "."

// PositionID is a position's identity in the API: <integration_id>.<instrument_id>.
//
// The natural key, and not a surrogate, because positions is a projection. L3 says it can
// be dropped and folded again to the same rows -- and a BIGSERIAL id would come back
// different every time, breaking every link a user saved and every alert that named one.
// The composite key is the same before and after a rebuild by construction (K42).
func PositionID(integrationID uuid.UUID, instrumentID int64) string {
	return integrationID.String() + idSeparator + strconv.FormatInt(instrumentID, 10)
}

// ParsePositionID splits an id back into the pair that addresses a row. It validates rather
// than trusts: the id arrives from a URL, and the account it is looked up under comes from
// the session, never from here.
func ParsePositionID(id string) (uuid.UUID, int64, error) {
	integration, instrument, found := strings.Cut(id, idSeparator)
	if !found {
		return uuid.Nil, 0, fmt.Errorf("%w: %q names no instrument", ErrMalformedID, id)
	}
	integrationID, err := uuid.Parse(integration)
	if err != nil {
		return uuid.Nil, 0, fmt.Errorf("%w: %q does not begin with an integration", ErrMalformedID, id)
	}
	instrumentID, err := strconv.ParseInt(instrument, 10, 64)
	if err != nil {
		return uuid.Nil, 0, fmt.Errorf("%w: %q does not end with an instrument", ErrMalformedID, id)
	}
	return integrationID, instrumentID, nil
}

// ID is the position's API identity.
func (h Holding) ID() string { return PositionID(h.IntegrationID, h.InstrumentID) }

// Find returns the holding with this id. A portfolio is small enough to scan, and scanning
// the same Portfolio the list endpoint returns is what keeps one position and the whole
// list built from one read -- so they can never disagree about the same number (L10).
func (p Portfolio) Find(id string) (Holding, bool) {
	for _, h := range p.Holdings {
		if h.ID() == id {
			return h, true
		}
	}
	return Holding{}, false
}
