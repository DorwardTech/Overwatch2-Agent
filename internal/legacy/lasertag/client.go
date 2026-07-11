// Package lasertag is a read-only HTTP client for the on-box "NOT-O-Zone"
// management app that fronts a legacy P&C Micros Nexus laser-tag server. That
// app is the only source of LIVE pack state on legacy sites: it fuses the
// NGServerPro COM bridge (online/playing/state per pack) with a PowerShell
// scrape of the server UI (resets/timeouts/charge/status), which no database
// exposes. The Overwatch legacy agent consumes its read endpoints; game
// history is read separately from the Nexus MySQL database (package nexus).
//
// Only the no-auth GET endpoints are used (health, status, current_game, and
// control.php?action=packs_live) — the legacy agent never issues control
// actions.
package lasertag

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client talks to the legacy management app's JSON API.
type Client struct {
	base   string
	client *http.Client
}

// New returns a client for the app's base URL (e.g. "http://192.168.0.50/lasertag").
func New(baseURL string) *Client {
	return &Client{
		base:   baseURL,
		client: &http.Client{Timeout: 8 * time.Second},
	}
}

// PackLive is one pack's live snapshot from control.php?action=packs_live.
// Battery/wifi/temperature are deliberately absent — the Nexus system does not
// report them.
type PackLive struct {
	PackID         int  `json:"-"`
	TeamID         int  `json:"team_id"`
	Blocked        bool `json:"blocked"`
	Online         bool `json:"online"`
	Allocated      bool `json:"allocated"`
	Playing        bool `json:"playing"`
	MemberLoggedOn bool `json:"member_logged_on"`
	State          int  `json:"state"`
	// Scraped from the NGServerPro Pack ListView (best-effort; may be absent).
	Scraped ScrapedPack `json:"scraped"`
}

// ScrapedPack holds the UI-scraped columns the COM bridge doesn't expose.
// Values arrive as strings from the scrape and are parsed leniently.
type ScrapedPack struct {
	Resets   *int   `json:"-"`
	Timeouts *int   `json:"-"`
	Charge   *int   `json:"-"`
	Status   string `json:"status"`
}

// packsLiveResponse is the raw packs_live envelope; pack keys are pack ids.
type packsLiveResponse struct {
	OK    bool                   `json:"ok"`
	Error string                 `json:"error"`
	Packs map[string]rawPackLive `json:"packs"`
}

type rawPackLive struct {
	TeamID         int        `json:"team_id"`
	Blocked        bool       `json:"blocked"`
	Online         bool       `json:"online"`
	Allocated      bool       `json:"allocated"`
	Playing        bool       `json:"playing"`
	MemberLoggedOn bool       `json:"member_logged_on"`
	State          int        `json:"state"`
	Scraped        rawScraped `json:"scraped"`
}

// rawScraped tolerates the scrape emitting numbers as strings ("12") or nulls.
type rawScraped struct {
	Resets   json.RawMessage `json:"resets"`
	Timeouts json.RawMessage `json:"timeouts"`
	Charge   json.RawMessage `json:"charge"`
	Status   json.RawMessage `json:"status"`
}

// PacksLive fetches the live pack snapshot. Packs are returned sorted by id.
func (c *Client) PacksLive(ctx context.Context) ([]PackLive, error) {
	var body packsLiveResponse
	if err := c.getJSON(ctx, "/api/control.php?action=packs_live", &body); err != nil {
		return nil, err
	}
	if !body.OK {
		return nil, fmt.Errorf("lasertag packs_live: %s", body.Error)
	}

	packs := make([]PackLive, 0, len(body.Packs))
	for key, r := range body.Packs {
		id, err := strconv.Atoi(key)
		if err != nil {
			continue
		}
		packs = append(packs, PackLive{
			PackID:         id,
			TeamID:         r.TeamID,
			Blocked:        r.Blocked,
			Online:         r.Online,
			Allocated:      r.Allocated,
			Playing:        r.Playing,
			MemberLoggedOn: r.MemberLoggedOn,
			State:          r.State,
			Scraped: ScrapedPack{
				Resets:   lenientInt(r.Scraped.Resets),
				Timeouts: lenientInt(r.Scraped.Timeouts),
				Charge:   lenientInt(r.Scraped.Charge),
				Status:   lenientString(r.Scraped.Status),
			},
		})
	}
	sortByID(packs)
	return packs, nil
}

// ServerStatus is the current game/server state from control.php?action=status.
type ServerStatus struct {
	OK          bool   `json:"ok"`
	ServerMode  int    `json:"server_mode"`
	GameID      int    `json:"game_id"`
	Profile     string `json:"profile"`
	GameName    string `json:"game_name"`
	Elapsed     int    `json:"server_elapsed"`
	Duration    int    `json:"duration"`
	Players     int    `json:"players"`
	PacksOnline int    `json:"packs_online"`
}

// Status fetches the live server/game status (light=1 skips the tasklist shell-out).
func (c *Client) Status(ctx context.Context) (ServerStatus, error) {
	var s ServerStatus
	err := c.getJSON(ctx, "/api/control.php?action=status&light=1", &s)
	return s, err
}

// Health reports whether the app and its database connections are up.
func (c *Client) Health(ctx context.Context) error {
	var body struct {
		OK bool `json:"ok"`
	}
	if err := c.getJSON(ctx, "/api/health.php", &body); err != nil {
		return err
	}
	if !body.OK {
		return fmt.Errorf("lasertag health reported not-ok")
	}
	return nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	u, err := url.Parse(c.base + path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "OverwatchAgent/2.0 (legacy)")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("lasertag %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
