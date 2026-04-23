// Package plan diffs the desired target RRSet set against the current
// Hetzner Cloud state.
package plan

import (
	"sort"
	"strings"

	"github.com/NothingTV/cf-to-hcloud-dns/internal/hetzner"
	"github.com/NothingTV/cf-to-hcloud-dns/internal/transform"
)

// Mode controls how an already-existing Hetzner zone is reconciled.
type Mode int

const (
	ModeZoneMissing Mode = iota
	ModeAdd
	ModeOverride
)

// OverrideOp replaces an existing Hetzner RRSet. The Before snapshot (values
// and TTL) is kept so a failure can be rolled back.
type OverrideOp struct {
	Before hetzner.RRSet
	After  transform.TargetRRSet
}

type Plan struct {
	CreateZone      bool
	ToCreate        []transform.TargetRRSet
	ToOverride      []OverrideOp
	ExactDuplicates []transform.TargetRRSet // present on both sides with identical value set and TTL
	Skipped         []transform.TargetRRSet // add-mode: (name,type) already on Hetzner, different values — left alone
}

// Build computes the plan. existing may be nil when ModeZoneMissing.
func Build(mode Mode, targets []transform.TargetRRSet, existing []hetzner.RRSet) Plan {
	p := Plan{CreateZone: mode == ModeZoneMissing}

	if mode == ModeZoneMissing {
		p.ToCreate = append(p.ToCreate, targets...)
		return p
	}

	byKey := map[string]hetzner.RRSet{}
	for _, e := range existing {
		byKey[key(e.Name, e.Type)] = e
	}

	for _, t := range targets {
		k := key(t.Name, t.Type)
		ex, found := byKey[k]
		if !found {
			p.ToCreate = append(p.ToCreate, t)
			continue
		}
		if sameRRSet(t, ex) {
			p.ExactDuplicates = append(p.ExactDuplicates, t)
			continue
		}
		if mode == ModeAdd {
			p.Skipped = append(p.Skipped, t)
			continue
		}
		p.ToOverride = append(p.ToOverride, OverrideOp{Before: ex, After: t})
	}
	return p
}

func key(name, typ string) string {
	return strings.ToLower(name) + "\x00" + strings.ToUpper(typ)
}

// sameRRSet compares the target RRSet to the Hetzner one by sorted value set
// and TTL. Names and types are assumed to match at call time.
func sameRRSet(t transform.TargetRRSet, e hetzner.RRSet) bool {
	if e.TTL == nil || *e.TTL != t.TTL {
		return false
	}
	if len(e.Records) != len(t.Values) {
		return false
	}
	ev := make([]string, len(e.Records))
	for i, r := range e.Records {
		ev[i] = r.Value
	}
	tv := append([]string(nil), t.Values...)
	sort.Strings(ev)
	sort.Strings(tv)
	for i := range ev {
		if ev[i] != tv[i] {
			return false
		}
	}
	return true
}
