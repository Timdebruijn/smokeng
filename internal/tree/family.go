package tree

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Family is the address family a series was measured over. It is part of a
// series' identity: one target can be measured over both families, and the two
// are different series that must never be pooled.
type Family uint8

const (
	// FamilyUnknown is history whose family cannot be established. It is
	// stored rather than guessed, so the data stays and is never attributed to
	// a family it may not have been measured over.
	FamilyUnknown Family = 0
	FamilyV4      Family = 4
	FamilyV6      Family = 6
)

// FamilyOf maps a target's address_family setting to a Family.
func FamilyOf(addressFamily string) Family {
	switch addressFamily {
	case "v4":
		return FamilyV4
	case "v6":
		return FamilyV6
	}
	return FamilyUnknown
}

func (f Family) String() string {
	switch f {
	case FamilyV4:
		return "v4"
	case FamilyV6:
		return "v6"
	}
	return "unknown"
}

// ParseFamily is the inverse of Family.String: exactly "v4", "v6" or "unknown".
// It is not FamilyOf, which reads a target's address_family setting and maps
// anything it does not know to unknown; a request that names a family it
// misspelled must be refused, not read as another series.
func ParseFamily(s string) (Family, bool) {
	switch s {
	case "v4":
		return FamilyV4, true
	case "v6":
		return FamilyV6, true
	case "unknown":
		return FamilyUnknown, true
	}
	return 0, false
}

// MarshalJSON and UnmarshalJSON make JSON carry a family the way the API's
// family parameter spells it ("v4"), not as the number it is stored as.
// UnmarshalJSON refuses anything ParseFamily refuses, a number included, so a
// body that misspells a family is an error and not another series.
func (f Family) MarshalJSON() ([]byte, error) { return json.Marshal(f.String()) }

func (f *Family) UnmarshalJSON(b []byte) error {
	var s string
	if string(b) == "null" {
		return nil // by convention, as for any type: null leaves the value as it is
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("family must be v4, v6 or unknown")
	}
	p, ok := ParseFamily(s)
	if !ok {
		return fmt.Errorf("family must be v4, v6 or unknown, got %q", s)
	}
	*f = p
	return nil
}

// Family is the family the target is measured over now; unknown for a group
// node, which has no address.
func (t *Target) Family() Family {
	if t.AddressFamily == nil {
		return FamilyUnknown
	}
	return FamilyOf(*t.AddressFamily)
}

// FamilyOf returns the current family of the target with the given id, and
// unknown when there is no such target.
func (t *Tree) FamilyOf(id int64) Family {
	if n, ok := t.nodes[id]; ok {
		return n.Family()
	}
	return FamilyUnknown
}
