// Package push delivers telemetry batches and game results to the Overwatch
// central server.
package push

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"time"
)

type Pusher struct {
	url         string // live telemetry ingest URL
	resultsURL  string // post-game results URL (derived from the ingest URL)
	commandsURL string // command-queue URL (derived from the ingest URL)
	ozoneURL    string // verbatim cache failover URL (derived from the ingest URL)
	token       string
	client      *http.Client
}

// OzoneGameMeta is the failover metadata central returns when listing or which
// the agent uploads alongside a verbatim game.
type OzoneGameMeta struct {
	GameNumber  int    `json:"game_number"`
	GameName    string `json:"game_name"`
	GameType    int    `json:"game_type"`
	Duration    int    `json:"duration"`
	StartTime   string `json:"start_time"`
	EndTime     string `json:"end_time"`
	PlayerCount int    `json:"player_count"`
	Valid       int    `json:"valid"`
}

// Command is a unit of work handed to the agent by central.
type Command struct {
	ID      int            `json:"id"`
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

// UnmarshalJSON tolerates a payload that isn't a JSON object (e.g. an empty
// array, null, or scalar): such payloads decode to an empty map rather than
// failing the whole command fetch.
func (c *Command) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID      int             `json:"id"`
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.ID = raw.ID
	c.Type = raw.Type
	c.Payload = map[string]any{}
	if len(raw.Payload) > 0 {
		_ = json.Unmarshal(raw.Payload, &c.Payload) // ignore non-object payloads
	}
	return nil
}

func New(ingestURL, token string) *Pusher {
	return &Pusher{
		url:         ingestURL,
		resultsURL:  deriveURL(ingestURL, "game-results"),
		commandsURL: deriveURL(ingestURL, "commands"),
		ozoneURL:    deriveURL(ingestURL, "ozone-games"),
		token:       token,
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// deriveURL turns ".../api/agent/ingest" into ".../api/agent/<endpoint>".
func deriveURL(ingestURL, endpoint string) string {
	u, err := url.Parse(ingestURL)
	if err != nil || u.Path == "" || u.Path == "/" {
		return ingestURL
	}
	u.Path = path.Dir(u.Path) + "/" + endpoint
	return u.String()
}

// Push POSTs a JSON telemetry batch. The idempotencyKey lets central dedupe retries.
func (p *Pusher) Push(payload []byte, idempotencyKey string) error {
	return p.post(p.url, payload, idempotencyKey)
}

// PushGameResults POSTs one completed game's raw O-Zone data to central.
func (p *Pusher) PushGameResults(gameNumber int, data map[string]any) error {
	payload, err := json.Marshal(map[string]any{"game_number": gameNumber, "data": data})
	if err != nil {
		return err
	}
	return p.post(p.resultsURL, payload, fmt.Sprintf("game-%d", gameNumber))
}

// PushOzoneGame backs up one game's VERBATIM payload to central's failover store.
// raw is sent as a JSON string so central preserves the exact bytes for restore.
func (p *Pusher) PushOzoneGame(meta OzoneGameMeta, raw []byte) error {
	payload, err := json.Marshal(map[string]any{
		"game_number": meta.GameNumber,
		"raw_json":    string(raw),
		"meta":        meta,
	})
	if err != nil {
		return err
	}
	return p.post(p.ozoneURL, payload, fmt.Sprintf("ozone-%d", meta.GameNumber))
}

// FetchOzoneGameMeta lists the games central holds for this site (for restore).
// A 404 (older central) is treated as "none".
func (p *Pusher) FetchOzoneGameMeta() ([]OzoneGameMeta, error) {
	resp, err := p.get(p.ozoneURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("central returned HTTP %d", resp.StatusCode)
	}
	var body struct {
		Games []OzoneGameMeta `json:"games"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Games, nil
}

// FetchOzoneGameRaw restores one game's verbatim payload from central.
func (p *Pusher) FetchOzoneGameRaw(gameNumber int) ([]byte, bool, error) {
	resp, err := p.get(fmt.Sprintf("%s/%d", p.ozoneURL, gameNumber))
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, false, fmt.Errorf("central returned HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// get issues an authenticated GET to a central agent endpoint.
func (p *Pusher) get(endpoint string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Agent-Token", p.token)
	req.Header.Set("User-Agent", "OverwatchAgent/2.0")
	return p.client.Do(req)
}

// FetchCommands pulls pending commands for this site. A 404 (older central, or
// the feature disabled) is treated as "no commands" rather than an error.
func (p *Pusher) FetchCommands() ([]Command, error) {
	req, err := http.NewRequest(http.MethodGet, p.commandsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Agent-Token", p.token)
	req.Header.Set("User-Agent", "OverwatchAgent/2.0")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("central returned HTTP %d", resp.StatusCode)
	}

	var body struct {
		Commands []Command `json:"commands"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Commands, nil
}

// AckCommand reports a command's outcome (status "acked" or "failed").
func (p *Pusher) AckCommand(id int, status string, result map[string]any) error {
	payload, err := json.Marshal(map[string]any{"status": status, "result": result})
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/%d/ack", p.commandsURL, id)
	return p.post(endpoint, payload, fmt.Sprintf("cmd-ack-%d", id))
}

func (p *Pusher) post(endpoint string, payload []byte, idempotencyKey string) error {
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Agent-Token", p.token)
	req.Header.Set("Idempotency-Key", idempotencyKey)
	req.Header.Set("User-Agent", "OverwatchAgent/2.0")

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("central returned HTTP %d", resp.StatusCode)
	}
	return nil
}
