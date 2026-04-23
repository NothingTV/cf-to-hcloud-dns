// Package transform converts Cloudflare DNS records into the target RRSet
// set that will be imported into Hetzner Cloud. It is a pure function with
// no I/O.
package transform

import (
	"sort"
	"strings"
)

// SourceRecord is the Cloudflare-side input shape.
type SourceRecord struct {
	Name     string
	Type     string
	Content  string
	TTL      int
	Proxied  bool
	Comment  string
	Priority *uint16 // used for MX / SRV
}

// TargetRRSet is a (name, type) group of record values with a single TTL —
// the shape Hetzner Cloud expects.
type TargetRRSet struct {
	Name   string
	Type   string
	TTL    int
	Values []string
}

type SkipReason struct {
	Record SourceRecord
	Reason string
}

// Hetzner Cloud supports this set of record types (see
// https://docs.hetzner.cloud/reference/cloud#tag/zone-rrsets).
// SOA is supported by the API but is owned by Hetzner for primary zones,
// so we always drop it during migration.
var hetznerSupported = map[string]bool{
	"A":     true,
	"AAAA":  true,
	"CAA":   true,
	"CNAME": true,
	"DS":    true,
	"HINFO": true,
	"HTTPS": true,
	"MX":    true,
	"NS":    true,
	"PTR":   true,
	"RP":    true,
	"SRV":   true,
	"SVCB":  true,
	"TLSA":  true,
	"TXT":   true,
}

// Transform applies sanitization rules and groups records into RRSets.
// Within an RRSet, the TTL is max(record TTLs, minTTL).
func Transform(zoneName string, records []SourceRecord, minTTL int) (rrsets []TargetRRSet, skipped []SkipReason) {
	zoneFQDN := strings.TrimSuffix(zoneName, ".") + "."

	type key struct{ name, typ string }
	groups := map[key]*TargetRRSet{}
	order := []key{}

	for _, r := range records {
		if strings.EqualFold(r.Type, "SOA") {
			skipped = append(skipped, SkipReason{r, "SOA is managed by Hetzner"})
			continue
		}
		if strings.EqualFold(r.Type, "NS") && isCloudflareNS(r) && isApex(r.Name, zoneFQDN) {
			skipped = append(skipped, SkipReason{r, "Cloudflare nameserver record"})
			continue
		}
		t := strings.ToUpper(r.Type)
		if !hetznerSupported[t] {
			skipped = append(skipped, SkipReason{r, "record type not supported by Hetzner Cloud DNS"})
			continue
		}

		ttl := r.TTL
		if ttl < minTTL {
			ttl = minTTL
		}
		value := normalizeValue(t, r)

		k := key{normalizeName(r.Name, zoneFQDN), t}
		g, ok := groups[k]
		if !ok {
			g = &TargetRRSet{Name: k.name, Type: k.typ, TTL: ttl}
			groups[k] = g
			order = append(order, k)
		}
		if ttl > g.TTL {
			g.TTL = ttl
		}
		if !contains(g.Values, value) {
			g.Values = append(g.Values, value)
		}
	}

	rrsets = make([]TargetRRSet, 0, len(order))
	for _, k := range order {
		g := groups[k]
		sort.Strings(g.Values)
		rrsets = append(rrsets, *g)
	}
	return rrsets, skipped
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func isCloudflareNS(r SourceRecord) bool {
	c := strings.ToLower(strings.TrimSuffix(r.Content, "."))
	return strings.HasSuffix(c, ".ns.cloudflare.com")
}

func isApex(name, zoneFQDN string) bool {
	n := strings.ToLower(strings.TrimSuffix(name, ".")) + "."
	return n == strings.ToLower(zoneFQDN)
}

// normalizeName converts "www.example.com" → "www", zone apex → "@".
func normalizeName(name, zoneFQDN string) string {
	n := strings.TrimSuffix(strings.ToLower(name), ".")
	z := strings.TrimSuffix(strings.ToLower(zoneFQDN), ".")
	if n == z {
		return "@"
	}
	if strings.HasSuffix(n, "."+z) {
		return strings.TrimSuffix(n, "."+z)
	}
	return n
}

// normalizeValue produces the RDATA string Hetzner Cloud expects for a given
// record type. Cloudflare stores priority separately for MX and SRV, and the
// content field format differs per record type, so we have to reassemble.
func normalizeValue(typ string, r SourceRecord) string {
	content := strings.TrimSpace(r.Content)
	switch typ {
	case "MX":
		// CF content is usually just the target. If it already leads with a
		// numeric priority we keep it; otherwise prepend r.Priority. MX target
		// must be a hostname, never numeric, so "first token is digits" is a
		// reliable signal that priority is already embedded.
		if firstFieldIsNumeric(content) {
			return withTrailingDotOnLastField(content)
		}
		if r.Priority != nil {
			return itoa(int(*r.Priority)) + " " + withTrailingDot(content)
		}
		return withTrailingDot(content)
	case "SRV":
		// SRV RDATA is "priority weight port target". CF's content is
		// "weight port target" (3 fields) with priority in a separate field.
		// Don't use a "leading digit" heuristic here — weight is numeric too.
		fields := strings.Fields(content)
		if len(fields) == 4 {
			return withTrailingDotOnLastField(content)
		}
		if len(fields) == 3 && r.Priority != nil {
			fields[2] = ensureTrailingDot(fields[2])
			return itoa(int(*r.Priority)) + " " + strings.Join(fields, " ")
		}
		return content
	default:
		return content
	}
}

func firstFieldIsNumeric(s string) bool {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return false
	}
	for _, c := range fields[0] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func withTrailingDot(s string) string { return ensureTrailingDot(strings.TrimSpace(s)) }

func withTrailingDotOnLastField(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return s
	}
	fields[len(fields)-1] = ensureTrailingDot(fields[len(fields)-1])
	return strings.Join(fields, " ")
}

func ensureTrailingDot(s string) string {
	if s == "" || strings.HasSuffix(s, ".") {
		return s
	}
	return s + "."
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
