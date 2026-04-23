// Package cloudflare is a minimal read-only client for the Cloudflare v4 API.
// We only need zone lookup and DNS record listing, so we avoid pulling in the
// full SDK.
package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/idna"
)

const defaultBaseURL = "https://api.cloudflare.com/client/v4"

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
	ID   string `json:"id"`
	Name string `json:"name"`
}

type DNSRecord struct {
	ID       string  `json:"id"`
	Type     string  `json:"type"`
	Name     string  `json:"name"`
	Content  string  `json:"content"`
	TTL      int     `json:"ttl"`
	Proxied  bool    `json:"proxied"`
	Comment  string  `json:"comment,omitempty"`
	Priority *uint16 `json:"priority,omitempty"`
}

type envelope struct {
	Success    bool              `json:"success"`
	Errors     []json.RawMessage `json:"errors"`
	Messages   []json.RawMessage `json:"messages"`
	ResultInfo struct {
		Page       int `json:"page"`
		PerPage    int `json:"per_page"`
		TotalPages int `json:"total_pages"`
		Count      int `json:"count"`
		TotalCount int `json:"total_count"`
	} `json:"result_info"`
	Result json.RawMessage `json:"result"`
}

// Ping performs a cheap authenticated call to validate the token.
func (c *Client) Ping(ctx context.Context) error {
	var out struct {
		Status string `json:"status"`
	}
	return c.get(ctx, "/user/tokens/verify", &out)
}

// FindZone looks up a zone by name. Cloudflare stores IDN zones under their
// Unicode form (e.g. "hülsbeck.de"), not punycode — so if the caller passes
// the ASCII/ACE form and it doesn't match, retry with the Unicode form.
func (c *Client) FindZone(ctx context.Context, name string) (*Zone, error) {
	if z, err := c.findZoneExact(ctx, name); err != nil || z != nil {
		return z, err
	}
	if unicodeName, err := idna.Lookup.ToUnicode(name); err == nil && unicodeName != name {
		return c.findZoneExact(ctx, unicodeName)
	}
	return nil, nil
}

func (c *Client) findZoneExact(ctx context.Context, name string) (*Zone, error) {
	q := url.Values{"name": {name}}
	var zones []Zone
	if err := c.get(ctx, "/zones?"+q.Encode(), &zones); err != nil {
		return nil, err
	}
	for i := range zones {
		if zones[i].Name == name {
			return &zones[i], nil
		}
	}
	return nil, nil
}

func (c *Client) ListRecords(ctx context.Context, zoneID string) ([]DNSRecord, error) {
	var all []DNSRecord
	page := 1
	for {
		q := url.Values{
			"page":     {fmt.Sprintf("%d", page)},
			"per_page": {"100"},
		}
		path := "/zones/" + zoneID + "/dns_records?" + q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Accept", "application/json")

		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		env, err := decodeEnvelope(resp)
		if err != nil {
			return nil, err
		}
		var batch []DNSRecord
		if err := json.Unmarshal(env.Result, &batch); err != nil {
			return nil, fmt.Errorf("decode records: %w", err)
		}
		all = append(all, batch...)
		if env.ResultInfo.TotalPages == 0 || page >= env.ResultInfo.TotalPages {
			break
		}
		page++
	}
	return all, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	env, err := decodeEnvelope(resp)
	if err != nil {
		return err
	}
	if out == nil || len(env.Result) == 0 || string(env.Result) == "null" {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

func decodeEnvelope(resp *http.Response) (*envelope, error) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cloudflare %s: %d %s: %s", resp.Request.URL.Path, resp.StatusCode, http.StatusText(resp.StatusCode), string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode cloudflare envelope: %w", err)
	}
	if !env.Success {
		return nil, fmt.Errorf("cloudflare api returned success=false: errors=%s", string(raw))
	}
	return &env, nil
}
