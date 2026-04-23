// Package migrate wires Cloudflare and Hetzner Cloud together and owns the
// rollback log. Only this package may delete or restore resources on
// Hetzner, and only for resources whose ids are in its own rollback log.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/NothingTV/cf-to-hcloud-dns/internal/cloudflare"
	"github.com/NothingTV/cf-to-hcloud-dns/internal/hetzner"
	"github.com/NothingTV/cf-to-hcloud-dns/internal/plan"
	"github.com/NothingTV/cf-to-hcloud-dns/internal/transform"
)

type Prompter interface {
	ExistingZoneChoice(zoneName string, existingCount int) (Choice, error)
	Confirm(prompt string) (bool, error)
}

type Choice int

const (
	ChoiceAdd Choice = iota
	ChoiceOverride
	ChoiceCancel
)

type Options struct {
	Domain string
	MinTTL int
	DryRun bool
	Yes    bool // skips final "Proceed?"; does NOT skip the existing-zone prompt
}

type Runner struct {
	CF       *cloudflare.Client
	HZ       *hetzner.Client
	Prompter Prompter
	Out      io.Writer
	Opts     Options
}

type rbEntry struct {
	createdZone  string // zone name we created (empty otherwise)
	createdRRSet *rrKey // RRSet we created
	overridden   *hetzner.RRSet
}

type rrKey struct{ Name, Type string }

type rollback struct{ entries []rbEntry }

func (r *rollback) pushCreatedZone(name string)     { r.entries = append(r.entries, rbEntry{createdZone: name}) }
func (r *rollback) pushCreatedRRSet(name, typ string) {
	k := rrKey{name, typ}
	r.entries = append(r.entries, rbEntry{createdRRSet: &k})
}
func (r *rollback) pushOverride(before hetzner.RRSet) {
	b := before
	r.entries = append(r.entries, rbEntry{overridden: &b})
}

func (r *Runner) Run(ctx context.Context) error {
	fmt.Fprintf(r.Out, "→ Validating credentials…\n")
	if err := r.CF.Ping(ctx); err != nil {
		return fmt.Errorf("cloudflare token check failed: %w", err)
	}
	if err := r.HZ.Ping(ctx); err != nil {
		return fmt.Errorf("hetzner token check failed: %w", err)
	}

	fmt.Fprintf(r.Out, "→ Fetching Cloudflare zone %q…\n", r.Opts.Domain)
	cfZone, err := r.CF.FindZone(ctx, r.Opts.Domain)
	if err != nil {
		return fmt.Errorf("cloudflare zone lookup: %w", err)
	}
	if cfZone == nil {
		return fmt.Errorf("cloudflare zone %q not found", r.Opts.Domain)
	}
	cfRecords, err := r.CF.ListRecords(ctx, cfZone.ID)
	if err != nil {
		return fmt.Errorf("list cloudflare records: %w", err)
	}

	sources := make([]transform.SourceRecord, 0, len(cfRecords))
	for _, rec := range cfRecords {
		sources = append(sources, transform.SourceRecord{
			Name: rec.Name, Type: rec.Type, Content: rec.Content, TTL: rec.TTL,
			Proxied: rec.Proxied, Comment: rec.Comment, Priority: rec.Priority,
		})
	}
	targets, skipped := transform.Transform(r.Opts.Domain, sources, r.Opts.MinTTL)
	for _, s := range skipped {
		fmt.Fprintf(r.Out, "  skip %s %s: %s\n", s.Record.Type, s.Record.Name, s.Reason)
	}
	fmt.Fprintf(r.Out, "→ %d RRSet(s) prepared, %d source record(s) skipped.\n", len(targets), len(skipped))

	fmt.Fprintf(r.Out, "→ Checking Hetzner Cloud for existing zone…\n")
	hzZone, err := r.HZ.FindZone(ctx, r.Opts.Domain)
	if err != nil {
		return fmt.Errorf("hetzner zone lookup: %w", err)
	}

	mode := plan.ModeZoneMissing
	var existing []hetzner.RRSet
	if hzZone != nil {
		existing, err = r.HZ.ListRRSets(ctx, r.Opts.Domain)
		if err != nil {
			return fmt.Errorf("list hetzner rrsets: %w", err)
		}
		fmt.Fprintf(r.Out, "  zone already exists with %d RRSet(s).\n", len(existing))
		choice, err := r.Prompter.ExistingZoneChoice(r.Opts.Domain, len(existing))
		if err != nil {
			return err
		}
		switch choice {
		case ChoiceAdd:
			mode = plan.ModeAdd
		case ChoiceOverride:
			mode = plan.ModeOverride
		case ChoiceCancel:
			fmt.Fprintf(r.Out, "Cancelled. No changes made.\n")
			return nil
		}
	}

	p := plan.Build(mode, targets, existing)
	r.printPlan(p)

	if r.Opts.DryRun {
		fmt.Fprintf(r.Out, "\nDry run: no changes applied.\n")
		return nil
	}
	if !r.Opts.Yes {
		ok, err := r.Prompter.Confirm("Proceed with apply?")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintf(r.Out, "Aborted.\n")
			return nil
		}
	}
	return r.apply(ctx, hzZone != nil, p)
}

func (r *Runner) printPlan(p plan.Plan) {
	fmt.Fprintf(r.Out, "\nPlan:\n")
	if p.CreateZone {
		fmt.Fprintf(r.Out, "  + create zone %s\n", r.Opts.Domain)
	}
	for _, t := range p.ToCreate {
		fmt.Fprintf(r.Out, "  + %-6s %-20s %v (ttl=%d)\n", t.Type, t.Name, t.Values, t.TTL)
	}
	for _, op := range p.ToOverride {
		fmt.Fprintf(r.Out, "  ~ %-6s %-20s %v → %v (ttl=%d)\n", op.After.Type, op.After.Name, recordValues(op.Before.Records), op.After.Values, op.After.TTL)
	}
	if len(p.ExactDuplicates) > 0 {
		fmt.Fprintf(r.Out, "  = %d exact duplicate RRSet(s) (no action)\n", len(p.ExactDuplicates))
	}
	if len(p.Skipped) > 0 {
		fmt.Fprintf(r.Out, "  ! %d RRSet(s) skipped (add mode — already present with different content):\n", len(p.Skipped))
		for _, s := range p.Skipped {
			fmt.Fprintf(r.Out, "      %s %s\n", s.Type, s.Name)
		}
	}
	if !p.CreateZone && len(p.ToCreate) == 0 && len(p.ToOverride) == 0 {
		fmt.Fprintf(r.Out, "  (nothing to do)\n")
	}
	fmt.Fprintln(r.Out)
}

func recordValues(rs []hetzner.Record) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Value
	}
	return out
}

func (r *Runner) apply(ctx context.Context, zoneExisted bool, p plan.Plan) error {
	rb := &rollback{}
	err := r.applyInner(ctx, zoneExisted, p, rb)
	if err == nil {
		fmt.Fprintf(r.Out, "✔ Done.\n")
		return nil
	}
	fmt.Fprintf(r.Out, "\n✖ Apply failed: %v\n→ Rolling back %d change(s)…\n", err, len(rb.entries))
	if rbErr := r.rollback(ctx, rb); rbErr != nil {
		return fmt.Errorf("apply failed: %w; ROLLBACK INCOMPLETE: %v", err, rbErr)
	}
	fmt.Fprintf(r.Out, "✔ Rolled back cleanly.\n")
	return err
}

func (r *Runner) applyInner(ctx context.Context, zoneExisted bool, p plan.Plan, rb *rollback) error {
	if p.CreateZone {
		if _, err := r.HZ.CreateZone(ctx, r.Opts.Domain, r.Opts.MinTTL); err != nil {
			return fmt.Errorf("create zone: %w", err)
		}
		rb.pushCreatedZone(r.Opts.Domain)
		fmt.Fprintf(r.Out, "  + zone %s\n", r.Opts.Domain)
	}
	if !zoneExisted && !p.CreateZone {
		return errors.New("internal error: zone neither existed nor was created")
	}

	for _, op := range p.ToOverride {
		rb.pushOverride(op.Before)
		records := toRecords(op.After.Values)
		if err := r.HZ.SetRecords(ctx, r.Opts.Domain, op.After.Name, op.After.Type, records); err != nil {
			return fmt.Errorf("override %s %s: %w", op.After.Type, op.After.Name, err)
		}
		// TTL change is a separate action.
		if op.Before.TTL == nil || *op.Before.TTL != op.After.TTL {
			if err := r.HZ.ChangeTTL(ctx, r.Opts.Domain, op.After.Name, op.After.Type, op.After.TTL); err != nil {
				return fmt.Errorf("change TTL %s %s: %w", op.After.Type, op.After.Name, err)
			}
		}
		fmt.Fprintf(r.Out, "  ~ %s %s %v\n", op.After.Type, op.After.Name, op.After.Values)
	}

	for _, t := range p.ToCreate {
		ttl := t.TTL
		rrset := hetzner.RRSet{
			Name: t.Name, Type: t.Type, TTL: &ttl, Records: toRecords(t.Values),
		}
		if _, err := r.HZ.CreateRRSet(ctx, r.Opts.Domain, rrset); err != nil {
			return fmt.Errorf("create %s %s: %w", t.Type, t.Name, err)
		}
		rb.pushCreatedRRSet(t.Name, t.Type)
		fmt.Fprintf(r.Out, "  + %s %s %v\n", t.Type, t.Name, t.Values)
	}
	return nil
}

func toRecords(values []string) []hetzner.Record {
	out := make([]hetzner.Record, len(values))
	for i, v := range values {
		out[i] = hetzner.Record{Value: v}
	}
	return out
}

func (r *Runner) rollback(ctx context.Context, rb *rollback) error {
	var failures []string
	for i := len(rb.entries) - 1; i >= 0; i-- {
		e := rb.entries[i]
		switch {
		case e.createdRRSet != nil:
			if err := r.HZ.DeleteRRSet(ctx, r.Opts.Domain, e.createdRRSet.Name, e.createdRRSet.Type); err != nil {
				failures = append(failures, fmt.Sprintf("delete rrset %s/%s: %v", e.createdRRSet.Name, e.createdRRSet.Type, err))
			}
		case e.overridden != nil:
			b := e.overridden
			if err := r.HZ.SetRecords(ctx, r.Opts.Domain, b.Name, b.Type, b.Records); err != nil {
				failures = append(failures, fmt.Sprintf("restore records %s/%s: %v", b.Name, b.Type, err))
				continue
			}
			if b.TTL != nil {
				if err := r.HZ.ChangeTTL(ctx, r.Opts.Domain, b.Name, b.Type, *b.TTL); err != nil {
					failures = append(failures, fmt.Sprintf("restore TTL %s/%s: %v", b.Name, b.Type, err))
				}
			}
		case e.createdZone != "":
			if err := r.HZ.DeleteZone(ctx, e.createdZone); err != nil {
				failures = append(failures, fmt.Sprintf("delete zone %s: %v", e.createdZone, err))
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("rollback failures: %v", failures)
	}
	return nil
}
