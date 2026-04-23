package plan

import (
	"testing"

	"github.com/NothingTV/cf-to-hcloud-dns/internal/hetzner"
	"github.com/NothingTV/cf-to-hcloud-dns/internal/transform"
)

func ttlPtr(v int) *int { return &v }

func TestBuild_ZoneMissing(t *testing.T) {
	targets := []transform.TargetRRSet{{Name: "@", Type: "A", TTL: 60, Values: []string{"1.2.3.4"}}}
	p := Build(ModeZoneMissing, targets, nil)
	if !p.CreateZone || len(p.ToCreate) != 1 {
		t.Fatalf("unexpected plan: %+v", p)
	}
}

func TestBuild_Add_SkipsConflicts_CreatesNew(t *testing.T) {
	targets := []transform.TargetRRSet{
		{Name: "@", Type: "A", TTL: 60, Values: []string{"9.9.9.9"}},
		{Name: "new", Type: "A", TTL: 60, Values: []string{"1.1.1.1"}},
	}
	existing := []hetzner.RRSet{
		{Name: "@", Type: "A", TTL: ttlPtr(60), Records: []hetzner.Record{{Value: "1.2.3.4"}}},
	}
	p := Build(ModeAdd, targets, existing)
	if len(p.ToCreate) != 1 || p.ToCreate[0].Name != "new" {
		t.Fatalf("expected only 'new' to be created: %+v", p)
	}
	if len(p.Skipped) != 1 || p.Skipped[0].Name != "@" {
		t.Fatalf("expected @/A to be skipped in add mode: %+v", p)
	}
	if len(p.ToOverride) != 0 {
		t.Fatalf("add mode must not override: %+v", p.ToOverride)
	}
}

func TestBuild_Override_ReplacesConflicts(t *testing.T) {
	targets := []transform.TargetRRSet{
		{Name: "@", Type: "A", TTL: 60, Values: []string{"9.9.9.9"}},
	}
	existing := []hetzner.RRSet{
		{Name: "@", Type: "A", TTL: ttlPtr(60), Records: []hetzner.Record{{Value: "1.2.3.4"}}},
		{Name: "mail", Type: "MX", TTL: ttlPtr(3600), Records: []hetzner.Record{{Value: "10 mx1"}}}, // untouched
	}
	p := Build(ModeOverride, targets, existing)
	if len(p.ToOverride) != 1 || p.ToOverride[0].Before.Name != "@" {
		t.Fatalf("expected 1 override of @/A: %+v", p.ToOverride)
	}
	if len(p.ToCreate) != 0 {
		t.Fatalf("expected no creates: %+v", p.ToCreate)
	}
}

func TestBuild_ExactDuplicate(t *testing.T) {
	targets := []transform.TargetRRSet{
		{Name: "@", Type: "A", TTL: 60, Values: []string{"1.2.3.4", "5.6.7.8"}},
	}
	existing := []hetzner.RRSet{
		{Name: "@", Type: "A", TTL: ttlPtr(60), Records: []hetzner.Record{{Value: "5.6.7.8"}, {Value: "1.2.3.4"}}},
	}
	p := Build(ModeOverride, targets, existing)
	if len(p.ExactDuplicates) != 1 || len(p.ToOverride) != 0 {
		t.Fatalf("expected exact duplicate, got %+v", p)
	}
}

func TestBuild_DifferentTTLIsNotExactDuplicate(t *testing.T) {
	targets := []transform.TargetRRSet{{Name: "@", Type: "A", TTL: 300, Values: []string{"1.2.3.4"}}}
	existing := []hetzner.RRSet{{Name: "@", Type: "A", TTL: ttlPtr(60), Records: []hetzner.Record{{Value: "1.2.3.4"}}}}
	p := Build(ModeOverride, targets, existing)
	if len(p.ToOverride) != 1 {
		t.Fatalf("TTL mismatch should force override: %+v", p)
	}
}
