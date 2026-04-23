// Package hetzner is a thin HTTP client for the Hetzner Cloud API, scoped to
// the Zones and RRSets endpoints (https://docs.hetzner.cloud/reference/cloud#tag/zones).
//
// DNS in Hetzner Cloud is modelled as RRSets: a set of records keyed by
// (name, type) with a single TTL and a list of values. That shape drives
// this client's surface.
package hetzner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const defaultBaseURL = "https://api.hetzner.cloud/v1"

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func New(token string) *Client {
	return &Client{
		BaseURL: defaultBaseURL,
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

type Zone struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Mode string `json:"mode"`
	TTL  int    `json:"ttl"`
}

type Record struct {
	Value   string `json:"value"`
	Comment string `json:"comment,omitempty"`
}

type RRSet struct {
	ID      string   `json:"id"` // "name/type"
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	TTL     *int     `json:"ttl,omitempty"`
	Records []Record `json:"records"`
}

// Ping validates the token with a cheap authenticated call.
func (c *Client) Ping(ctx context.Context) error {
	var resp struct{}
	return c.do(ctx, http.MethodGet, "/zones?per_page=1", nil, &resp)
}

// FindZone returns the zone with the given name, or nil if none exists.
func (c *Client) FindZone(ctx context.Context, name string) (*Zone, error) {
	q := url.Values{"name": {name}}
	var resp struct {
		Zones []Zone `json:"zones"`
	}
	if err := c.do(ctx, http.MethodGet, "/zones?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	for i := range resp.Zones {
		if resp.Zones[i].Name == name {
			return &resp.Zones[i], nil
		}
	}
	return nil, nil
}

// CreateZone creates a Primary zone. ttl is the zone's default TTL for RRSets
// that do not set their own.
func (c *Client) CreateZone(ctx context.Context, name string, ttl int) (*Zone, error) {
	body := map[string]any{"name": name, "mode": "primary", "ttl": ttl}
	var resp struct {
		Zone Zone `json:"zone"`
	}
	if err := c.do(ctx, http.MethodPost, "/zones", body, &resp); err != nil {
		return nil, err
	}
	return &resp.Zone, nil
}

// DeleteZone removes a zone. The API returns an Action; we don't track it.
func (c *Client) DeleteZone(ctx context.Context, nameOrID string) error {
	return c.do(ctx, http.MethodDelete, "/zones/"+nameOrID, nil, nil)
}

// ListRRSets paginates through every RRSet in a zone.
func (c *Client) ListRRSets(ctx context.Context, zone string) ([]RRSet, error) {
	var out []RRSet
	page := 1
	for {
		q := url.Values{"page": {fmt.Sprintf("%d", page)}, "per_page": {"50"}}
		path := "/zones/" + zone + "/rrsets?" + q.Encode()
		var resp struct {
			RRSets []RRSet `json:"rrsets"`
			Meta   struct {
				Pagination struct {
					Page         int `json:"page"`
					LastPage     int `json:"last_page"`
					TotalEntries int `json:"total_entries"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.RRSets...)
		if resp.Meta.Pagination.LastPage == 0 || page >= resp.Meta.Pagination.LastPage {
			break
		}
		page++
	}
	return out, nil
}

// CreateRRSet creates a new RRSet in the zone.
func (c *Client) CreateRRSet(ctx context.Context, zone string, rr RRSet) (*RRSet, error) {
	body := map[string]any{
		"name":    rr.Name,
		"type":    rr.Type,
		"records": rr.Records,
	}
	if rr.TTL != nil {
		body["ttl"] = *rr.TTL
	}
	var resp struct {
		RRSet RRSet `json:"rrset"`
	}
	path := "/zones/" + zone + "/rrsets"
	if err := c.do(ctx, http.MethodPost, path, body, &resp); err != nil {
		return nil, err
	}
	return &resp.RRSet, nil
}

// DeleteRRSet removes the RRSet identified by (name, type).
func (c *Client) DeleteRRSet(ctx context.Context, zone, name, typ string) error {
	return c.do(ctx, http.MethodDelete, rrsetPath(zone, name, typ), nil, nil)
}

// SetRecords replaces the list of records of an existing RRSet. The RRSet
// itself (name, type, TTL, labels) is not touched.
func (c *Client) SetRecords(ctx context.Context, zone, name, typ string, records []Record) error {
	body := map[string]any{"records": records}
	return c.do(ctx, http.MethodPost, rrsetPath(zone, name, typ)+"/actions/set_records", body, nil)
}

// ChangeTTL updates the TTL of an existing RRSet.
func (c *Client) ChangeTTL(ctx context.Context, zone, name, typ string, ttl int) error {
	body := map[string]any{"ttl": ttl}
	return c.do(ctx, http.MethodPost, rrsetPath(zone, name, typ)+"/actions/change_ttl", body, nil)
}

// rrsetPath builds the RRSet URL path segment. Hetzner Cloud expects names
// like `*` and `@` in their literal form — %-encoding them (e.g. %2A, %40)
// causes 404s because the API does not decode them. DNS names and types are
// already constrained to URL-safe characters, so no escaping is needed.
func rrsetPath(zone, name, typ string) string {
	return "/zones/" + zone + "/rrsets/" + name + "/" + typ
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("hetzner %s %s: %d %s: %s", method, path, resp.StatusCode, http.StatusText(resp.StatusCode), string(raw))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
