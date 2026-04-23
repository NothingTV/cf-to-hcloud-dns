package transform

import (
	"reflect"
	"sort"
	"testing"
)

func u16(v uint16) *uint16 { return &v }

func byName(rs []TargetRRSet) []TargetRRSet {
	out := append([]TargetRRSet(nil), rs...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Type < out[j].Type
	})
	return out
}

func TestTransform_GroupsIntoRRSets(t *testing.T) {
	zone := "example.com"
	records := []SourceRecord{
		{Name: "example.com", Type: "SOA", Content: "ns.cloudflare.com. dns.cloudflare.com. 1 10000 2400 604800 3600", TTL: 3600},
		{Name: "example.com", Type: "NS", Content: "alice.ns.cloudflare.com", TTL: 86400},
		{Name: "example.com", Type: "NS", Content: "bob.ns.cloudflare.com.", TTL: 86400},
		{Name: "example.com", Type: "A", Content: "1.2.3.4", TTL: 1, Comment: "primary"},
		{Name: "example.com", Type: "A", Content: "5.6.7.8", TTL: 300}, // second value, same RRSet
		{Name: "www.example.com", Type: "A", Content: "1.2.3.4", TTL: 300},
		{Name: "example.com", Type: "MX", Content: "mail.example.com", TTL: 3600, Priority: u16(10)},
		{Name: "example.com", Type: "TXT", Content: "\"v=spf1 -all\"", TTL: 60},
		{Name: "example.com", Type: "SSHFP", Content: "1 1 deadbeef", TTL: 300},
	}

	got, skipped := Transform(zone, records, 60)
	want := []TargetRRSet{
		{Name: "@", Type: "A", TTL: 300, Values: []string{"1.2.3.4", "5.6.7.8"}}, // TTL = max of group, clamped
		{Name: "@", Type: "MX", TTL: 3600, Values: []string{"10 mail.example.com."}},
		{Name: "@", Type: "TXT", TTL: 60, Values: []string{"\"v=spf1 -all\""}},
		{Name: "www", Type: "A", TTL: 300, Values: []string{"1.2.3.4"}},
	}
	if !reflect.DeepEqual(byName(got), byName(want)) {
		t.Fatalf("rrsets mismatch\ngot:  %#v\nwant: %#v", byName(got), byName(want))
	}
	if len(skipped) != 4 {
		t.Fatalf("expected 4 skipped, got %d: %#v", len(skipped), skipped)
	}
}

func TestTransform_MXPriorityNotDoubled(t *testing.T) {
	got, _ := Transform("example.com", []SourceRecord{
		{Name: "example.com", Type: "MX", Content: "10 mail.example.com", TTL: 3600, Priority: u16(10)},
	}, 60)
	if len(got) != 1 || got[0].Values[0] != "10 mail.example.com." {
		t.Fatalf("priority should not be double-prefixed: %#v", got)
	}
}

func TestTransform_SRVAssemblesFullRDATA(t *testing.T) {
	// Cloudflare's SRV content is "weight port target"; priority is separate.
	got, _ := Transform("example.com", []SourceRecord{
		{Name: "_autodiscover._tcp.example.com", Type: "SRV", Content: "1 443 salem.nothingtv.de", TTL: 3600, Priority: u16(1)},
	}, 60)
	if len(got) != 1 {
		t.Fatalf("expected 1 RRSet: %#v", got)
	}
	want := "1 1 443 salem.nothingtv.de."
	if got[0].Values[0] != want {
		t.Fatalf("SRV RDATA: got %q, want %q", got[0].Values[0], want)
	}
}

func TestTransform_SRVAlreadyHasPriority(t *testing.T) {
	got, _ := Transform("example.com", []SourceRecord{
		{Name: "_sip._tcp.example.com", Type: "SRV", Content: "10 20 5060 sip.example.com.", TTL: 3600, Priority: u16(10)},
	}, 60)
	if got[0].Values[0] != "10 20 5060 sip.example.com." {
		t.Fatalf("SRV with full RDATA unchanged: %q", got[0].Values[0])
	}
}

func TestTransform_SubdomainNSKept(t *testing.T) {
	got, _ := Transform("example.com", []SourceRecord{
		{Name: "sub.example.com", Type: "NS", Content: "ns1.other.net", TTL: 86400},
	}, 60)
	if len(got) != 1 || got[0].Name != "sub" || got[0].Type != "NS" {
		t.Fatalf("expected sub NS RRSet: %#v", got)
	}
}
