package setupui

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"overwatch/agent/internal/ozonelib"
	"overwatch/agent/internal/ozonesim"
)

func newTestServer(t *testing.T, configPath string) (*Server, http.Handler) {
	t.Helper()
	s, err := New(configPath, filepath.Dir(configPath), filepath.Join(filepath.Dir(configPath), "agent.log"), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, s.handler(func() {})
}

func do(t *testing.T, h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Host = "127.0.0.1:9999"
	if key != "" {
		r.Header.Set("X-Setup-Key", key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return body
}

// The page can write the site token and start services, so getting in has to
// take the key that was handed to the browser — and only over the loopback
// address, addressed by IP.
func TestAccessControl(t *testing.T) {
	s, h := newTestServer(t, filepath.Join(t.TempDir(), "agent.env"))

	t.Run("no key is refused", func(t *testing.T) {
		if got := do(t, h, http.MethodGet, "/api/config", "", "").Code; got != http.StatusForbidden {
			t.Errorf("status = %d, want 403", got)
		}
	})

	t.Run("a wrong key is refused", func(t *testing.T) {
		if got := do(t, h, http.MethodGet, "/api/config", "not-the-key", "").Code; got != http.StatusForbidden {
			t.Errorf("status = %d, want 403", got)
		}
	})

	t.Run("the right key is let in", func(t *testing.T) {
		if got := do(t, h, http.MethodGet, "/api/config", s.key, "").Code; got != http.StatusOK {
			t.Errorf("status = %d, want 200", got)
		}
	})

	// A name that resolves to 127.0.0.1 is how DNS rebinding gets a browser to
	// treat this server as its own origin.
	t.Run("a hostname is refused even with the key", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/config", nil)
		r.Host = "rebind.example.com"
		r.Header.Set("X-Setup-Key", s.key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})

	t.Run("the page itself needs the key in the address", func(t *testing.T) {
		if got := do(t, h, http.MethodGet, "/", "", "").Code; got != http.StatusForbidden {
			t.Errorf("status = %d, want 403", got)
		}
		if got := do(t, h, http.MethodGet, "/?key="+s.key, "", "").Code; got != http.StatusOK {
			t.Errorf("status with the key = %d, want 200", got)
		}
	})
}

func TestReadsAndWritesTheConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.env")
	existing := "# venue note\nCENTRAL_API_URL=https://old.example/api/agent/ingest\nNEXUS_DSN=keep:me@tcp(h)/db\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	s, h := newTestServer(t, path)

	got := decode(t, do(t, h, http.MethodGet, "/api/config", s.key, ""))
	values, _ := got["values"].(map[string]any)
	if values["CENTRAL_API_URL"] != "https://old.example/api/agent/ingest" {
		t.Fatalf("the page did not read the existing settings: %v", values)
	}

	body := `{"CENTRAL_API_URL":"https://ow2.example/api/agent/ingest","AGENT_TOKEN":"OW2_1_abc",
	  "OZONE_WS_HOST":"127.0.0.1","OZONE_WS_PORT":"12113","ENABLE_GAME_RESULTS":"true",
	  "ENABLE_CACHE":"false","ENABLE_PROXY":"false","ENABLE_MSG_BUS":"false","PROXY_LISTEN_ADDR":"",
	  "ADMIN_API_ADDR":"","ADMIN_API_TOKEN":"","POLL_INTERVAL":"5","IDLE_POLL_INTERVAL":"15",
	  "SLOW_POLL_INTERVAL":"60","HEALTH_ADDR":"127.0.0.1:8088"}`
	if w := do(t, h, http.MethodPost, "/api/config", s.key, body); w.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)
	for _, want := range []string{
		"# venue note", // the operator's comment survives
		// The form manages NEXUS_DSN now, and this save is an O-Zone one, so the
		// legacy setting is parked behind a comment rather than deleted: the
		// venue's database password is still in the file, and switching the mode
		// back turns the same line on again.
		"# NEXUS_DSN=keep:me@tcp(h)/db",
		"AGENT_MODE=ozone",
		"CENTRAL_API_URL=https://ow2.example/api/agent/ingest", // and the change landed
		"AGENT_TOKEN=OW2_1_abc",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("saved file is missing %q:\n%s", want, text)
		}
	}

	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the file holding the token has permissions %o, want 600", perm)
	}
}

func TestSaveRejectsSettingsTheAgentCouldNotUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.env")
	s, h := newTestServer(t, path)

	body := `{"CENTRAL_API_URL":"ow2.example","AGENT_TOKEN":"","OZONE_WS_HOST":"","OZONE_WS_PORT":"twelve",
	  "ENABLE_GAME_RESULTS":"true","ENABLE_CACHE":"false","ENABLE_PROXY":"false","ENABLE_MSG_BUS":"false",
	  "PROXY_LISTEN_ADDR":"12124","ADMIN_API_ADDR":"","ADMIN_API_TOKEN":"","POLL_INTERVAL":"5",
	  "IDLE_POLL_INTERVAL":"15","SLOW_POLL_INTERVAL":"60","HEALTH_ADDR":""}`
	w := do(t, h, http.MethodPost, "/api/config", s.key, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	problems, _ := decode(t, w)["problems"].([]any)
	if len(problems) < 4 {
		t.Errorf("expected a problem for the address, the token, the game server and the port; got %v", problems)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a rejected save still wrote the file")
	}
}

// The test button must say which of the three things is wrong, because that is
// the whole reason it exists.
func TestCentralTestExplainsTheFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		ok     bool
		expect string
	}{
		{"accepted", http.StatusOK, true, "token was accepted"},
		{"rejected token", http.StatusForbidden, false, "rejected this site token"},
		{"unauthorised", http.StatusUnauthorized, false, "rejected this site token"},
		{"wrong path", http.StatusNotFound, false, "/api/agent/ingest"},
		{"rate limited", http.StatusTooManyRequests, false, "rate-limiting"},
		{"server error", http.StatusInternalServerError, false, "HTTP 500"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/commands") {
					t.Errorf("the probe hit %s; it must only read the command queue", r.URL.Path)
				}
				if r.Method != http.MethodGet {
					t.Errorf("the probe used %s; it must not write anything", r.Method)
				}
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(`{"commands":[]}`))
			}))
			defer central.Close()

			s, h := newTestServer(t, filepath.Join(t.TempDir(), "agent.env"))
			body, _ := json.Marshal(map[string]string{"url": central.URL + "/api/agent/ingest", "token": "OW2_1_abc"})
			got := decode(t, do(t, h, http.MethodPost, "/api/test/central", s.key, string(body)))

			if got["ok"] != c.ok {
				t.Errorf("ok = %v, want %v (%v)", got["ok"], c.ok, got["message"])
			}
			if msg, _ := got["message"].(string); !strings.Contains(msg, c.expect) {
				t.Errorf("message = %q, want it to mention %q", msg, c.expect)
			}
		})
	}
}

func TestCentralTestReportsAnUnreachableServer(t *testing.T) {
	s, h := newTestServer(t, filepath.Join(t.TempDir(), "agent.env"))
	body := `{"url":"http://127.0.0.1:1/api/agent/ingest","token":"OW2_1_abc"}`
	got := decode(t, do(t, h, http.MethodPost, "/api/test/central", s.key, body))
	if got["ok"] != false {
		t.Fatalf("ok = %v, want false", got["ok"])
	}
	if msg, _ := got["message"].(string); !strings.Contains(msg, "Could not reach Overwatch") {
		t.Errorf("message = %q", msg)
	}
}

func TestLogTail(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	s, err := New(filepath.Join(dir, "agent.env"), dir, logPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := s.handler(func() {})

	got := decode(t, do(t, h, http.MethodGet, "/api/log", s.key, ""))
	if note, _ := got["note"].(string); !strings.Contains(note, "does not exist") {
		t.Errorf("a missing log should say so, got %v", got)
	}

	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString("line ")
		sb.WriteString(strings.Repeat("x", 100))
		sb.WriteString("\n")
	}
	if err := os.WriteFile(logPath, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got = decode(t, do(t, h, http.MethodGet, "/api/log", s.key, ""))
	lines, _ := got["lines"].([]any)
	if len(lines) == 0 || len(lines) > 40 {
		t.Errorf("returned %d lines, want between 1 and 40", len(lines))
	}
}

func TestServiceIsReportedUnsupportedWithoutOne(t *testing.T) {
	s, h := newTestServer(t, filepath.Join(t.TempDir(), "agent.env"))
	got := decode(t, do(t, h, http.MethodGet, "/api/service", s.key, ""))
	if got["supported"] != false {
		t.Errorf("supported = %v, want false", got["supported"])
	}
}

func TestFinishStopsTheServer(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "agent.env"), t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := false
	h := s.handler(func() { stopped = true })
	_ = ctx

	if w := do(t, h, http.MethodPost, "/api/finish", s.key, ""); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !stopped {
		t.Error("finishing did not stop the server")
	}
}

// The test buttons dial what they are given, so what they are given has to be
// an address the agent would actually have been configured with — and when it
// is not, the page has to say which part of it is wrong.
func TestCentralTestRejectsAnAddressTheAgentCouldNotUse(t *testing.T) {
	cases := []struct {
		name, url, expect string
	}{
		{"no scheme", "overwatch.example.com/api/agent/ingest", "must start with https://"},
		{"wrong scheme", "file:///etc/passwd", "must start with https://"},
		{"no host", "https:///api/agent/ingest", "missing the server name"},
		{"credentials in the address", "https://someone:secret@overwatch.example.com/api/agent/ingest", "Remove the username and password"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, h := newTestServer(t, filepath.Join(t.TempDir(), "agent.env"))
			body, _ := json.Marshal(map[string]string{"url": c.url, "token": "OW2_1_abc"})
			got := decode(t, do(t, h, http.MethodPost, "/api/test/central", s.key, string(body)))

			if got["ok"] != false {
				t.Fatalf("ok = %v, want false", got["ok"])
			}
			if msg, _ := got["message"].(string); !strings.Contains(msg, c.expect) {
				t.Errorf("message = %q, want it to mention %q", msg, c.expect)
			}
		})
	}
}

func TestGameServerTestRejectsAnAddressTheAgentCouldNotDial(t *testing.T) {
	cases := []struct {
		name, host, port, expect string
	}{
		{"a web address", "http://127.0.0.1", "12113", "not a web address"},
		{"a path", "127.0.0.1/telemetry", "12113", "not a web address"},
		{"host and port together", "127.0.0.1:12113", "12113", "port field"},
		{"port out of range", "127.0.0.1", "99999", "between 1 and 65535"},
		{"port not a number", "127.0.0.1", "twelve", "between 1 and 65535"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, h := newTestServer(t, filepath.Join(t.TempDir(), "agent.env"))
			body, _ := json.Marshal(map[string]string{"host": c.host, "port": c.port})
			got := decode(t, do(t, h, http.MethodPost, "/api/test/gameserver", s.key, string(body)))

			if got["ok"] != false {
				t.Fatalf("ok = %v, want false", got["ok"])
			}
			if msg, _ := got["message"].(string); !strings.Contains(msg, c.expect) {
				t.Errorf("message = %q, want it to mention %q", msg, c.expect)
			}
		})
	}
}

// An IPv6 literal has colons in it and is a perfectly good address to dial;
// the check that catches "host:port" must not catch it.
func TestGameServerTestAcceptsAnIPv6Address(t *testing.T) {
	for _, host := range []string{"::1", "2001:db8::1", "127.0.0.1", "ozone-pc.venue.local"} {
		if problem := gameServerProblem(host, "12113"); problem != "" {
			t.Errorf("gameServerProblem(%q) = %q, want no problem", host, problem)
		}
	}
}

// A Nexus venue has no O-Zone server to name. Before the page understood the
// mode it required one anyway, so the form could not be submitted at all —
// which is the whole reason legacy mode was unreachable from the installer.
func TestLegacyModeDoesNotRequireAGameServer(t *testing.T) {
	legacy := settings{
		CentralURL:  "https://ow2.example/api/agent/ingest",
		Token:       "OW2_1_abc",
		Mode:        "legacy",
		NexusDSN:    "ow2ro:pw@tcp(10.0.0.2:3306)/ng_system",
		LasertagURL: "http://10.0.0.2/lasertag",
	}
	if problems := legacy.validate(); len(problems) > 0 {
		t.Fatalf("a complete legacy venue was rejected: %v", problems)
	}
	// The same settings without the mode are an O-Zone venue missing its server.
	ozone := legacy
	ozone.Mode = "ozone"
	if problems := ozone.validate(); len(problems) == 0 {
		t.Fatal("an O-Zone venue with no game server address was accepted")
	}
}

func TestLegacyModeRequiresItsOwnTwoSettings(t *testing.T) {
	base := settings{
		CentralURL: "https://ow2.example/api/agent/ingest",
		Token:      "OW2_1_abc",
		Mode:       "legacy",
	}
	for _, tc := range []struct {
		name string
		in   settings
		want string
	}{
		{"neither", base, "Nexus database"},
		{"no app", func() settings { s := base; s.NexusDSN = "u:p@tcp(h:3306)/ng_system"; return s }(), "management app"},
		{"no database", func() settings { s := base; s.LasertagURL = "http://h/lasertag"; return s }(), "Nexus database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := tc.in.validate()
			if len(problems) == 0 {
				t.Fatal("an incomplete legacy venue was accepted")
			}
			if !strings.Contains(strings.Join(problems, " "), tc.want) {
				t.Errorf("problems %v do not mention %q", problems, tc.want)
			}
		})
	}
}

func TestNexusDSNProblem(t *testing.T) {
	for _, tc := range []struct{ name, dsn, want string }{
		{"good", "ow2ro:pw@tcp(10.0.0.2:3306)/ng_system", ""},
		{"good with params", "ow2ro:pw@tcp(h:3306)/ng_system?parseTime=true", ""},
		{"jdbc style", "mysql://ow2ro:pw@10.0.0.2/ng_system", "mysql://"},
		{"no at", "ow2ro-pw-tcp(h)/ng_system", "missing its @"},
		{"no credentials", "@tcp(h:3306)/ng_system", "username and password"},
		{"bare host", "ow2ro:pw@10.0.0.2/ng_system", "tcp(...)"},
		{"no database", "ow2ro:pw@tcp(10.0.0.2:3306)", "database name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nexusDSNProblem(tc.dsn)
			if tc.want == "" && got != "" {
				t.Fatalf("a usable DSN was rejected: %s", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("problem for %q = %q, want it to mention %q", tc.dsn, got, tc.want)
			}
		})
	}
}

func TestLasertagURLProblem(t *testing.T) {
	for _, tc := range []struct{ name, url, want string }{
		{"good", "http://10.0.0.2/lasertag", ""},
		{"good https", "https://venue.example/lasertag", ""},
		{"no scheme", "10.0.0.2/lasertag", "http://"},
		{"trailing slash", "http://10.0.0.2/lasertag/", "trailing slash"},
		{"a page", "http://10.0.0.2/lasertag/api/health.php", "not one of its pages"},
		{"credentials", "http://u:p@10.0.0.2/lasertag", "Remove the username"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lasertagURLProblem(tc.url)
			if tc.want == "" && got != "" {
				t.Fatalf("a usable address was rejected: %s", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("problem for %q = %q, want it to mention %q", tc.url, got, tc.want)
			}
		})
	}
}

// Switching an existing O-Zone venue to legacy must leave it configured for
// legacy and nothing else: the O-Zone settings parked, the Nexus pair live.
func TestSwitchingModeParksTheOtherModesSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.env")
	existing := "CENTRAL_API_URL=https://ow2.example/api/agent/ingest\nOZONE_WS_HOST=127.0.0.1\nENABLE_CACHE=true\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	s, h := newTestServer(t, path)

	body := `{"CENTRAL_API_URL":"https://ow2.example/api/agent/ingest","AGENT_TOKEN":"OW2_1_abc",
	  "AGENT_MODE":"legacy","NEXUS_DSN":"ow2ro:pw@tcp(10.0.0.2:3306)/ng_system",
	  "LASERTAG_URL":"http://10.0.0.2/lasertag","GAME_SYNC_INTERVAL":"30",
	  "OZONE_WS_HOST":"","OZONE_WS_PORT":"","ENABLE_GAME_RESULTS":"","ENABLE_CACHE":"",
	  "ENABLE_PROXY":"","ENABLE_MSG_BUS":"","PROXY_LISTEN_ADDR":""}`
	if w := do(t, h, http.MethodPost, "/api/config", s.key, body); w.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)
	for _, want := range []string{
		"AGENT_MODE=legacy",
		"NEXUS_DSN=ow2ro:pw@tcp(10.0.0.2:3306)/ng_system",
		"LASERTAG_URL=http://10.0.0.2/lasertag",
		"# OZONE_WS_HOST=127.0.0.1", // parked, not deleted
		"# ENABLE_CACHE=true",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("saved file is missing %q:\n%s", want, text)
		}
	}

	// And the page reads its own file back as a legacy venue.
	got := decode(t, do(t, h, http.MethodGet, "/api/config", s.key, ""))
	values, _ := got["values"].(map[string]any)
	if values["AGENT_MODE"] != "legacy" {
		t.Errorf("the page read the mode back as %v, want legacy", values["AGENT_MODE"])
	}
	if values["OZONE_WS_HOST"] != nil {
		t.Errorf("a parked setting was read back as live: %v", values["OZONE_WS_HOST"])
	}
}

// A venue installed through the wizard used to get no cache, no proxy and no
// failover restore, because the checkbox that turns all three on started
// unticked and nobody on shift had a reason to tick it. It starts ticked now,
// for a venue that has never been configured.
//
// This asserts on the shipped page rather than on behaviour, because the
// defaulting happens in the browser: what it can prove is that the cache
// checkbox is defaulted the same way the scoresheet one is — "on unless the
// file says otherwise" — and that a saved value still wins. What it cannot
// prove is that the page renders; the render test above covers that.
func TestTheCacheCheckboxDefaultsOnForANewVenue(t *testing.T) {
	page, err := uiFS.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	text := string(page)

	for _, want := range []string{
		`$("cache").checked = v.ENABLE_CACHE === undefined ? true : truthy(v.ENABLE_CACHE);`,
		`$("results").checked = v.ENABLE_GAME_RESULTS === undefined ? true : truthy(v.ENABLE_GAME_RESULTS);`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the page no longer defaults a setting on for a new venue:\n  want %s", want)
		}
	}

	// The unticked box must still turn all three off, or "leave it off" on a
	// machine that should open no ports would not mean anything.
	for _, want := range []string{
		`s.ENABLE_CACHE = String(cache);`,
		`s.ENABLE_PROXY = String(cache);`,
		`s.ENABLE_MSG_BUS = String(cache);`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the cache checkbox no longer drives all three settings:\n  want %s", want)
		}
	}
}

func TestThePageRenders(t *testing.T) {
	s, h := newTestServer(t, filepath.Join(t.TempDir(), "agent.env"))
	w := do(t, h, http.MethodGet, "/?key="+s.key, "", "")
	body := w.Body.String()
	// A template error stops the output part-way, after the status is sent.
	for _, want := range []string{`const KEY = "` + s.key + `"`, `id="find"`, `id="ozoneSid"`, "</html>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the rendered page is missing %q", want)
		}
	}
}

// --- auto, the site and the arena ---

func TestAutoIsSavedAsOneWord(t *testing.T) {
	for _, host := range []string{"auto", "AUTO", " Auto "} {
		t.Run(host, func(t *testing.T) {
			in := settings{CentralURL: "https://ow2.example/api/agent/ingest", Token: "OW2_1_abc", OzoneHost: host}
			if problems := in.validate(); len(problems) > 0 {
				t.Fatalf("auto was rejected: %v", problems)
			}
			path := filepath.Join(t.TempDir(), "agent.env")
			s, h := newTestServer(t, path)
			body, _ := json.Marshal(map[string]string{
				"CENTRAL_API_URL": "https://ow2.example/api/agent/ingest", "AGENT_TOKEN": "OW2_1_abc",
				"OZONE_WS_HOST": host, "OZONE_WS_PORT": "12113",
			})
			if w := do(t, h, http.MethodPost, "/api/config", s.key, string(body)); w.Code != http.StatusOK {
				t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !regexp.MustCompile(`(?m)^OZONE_WS_HOST=auto$`).Match(saved) {
				t.Errorf("auto typed as %q was not saved as auto:\n%s", host, saved)
			}
		})
	}
}

func TestSiteAndArenaAreChecked(t *testing.T) {
	base := settings{CentralURL: "https://ow2.example/api/agent/ingest", Token: "OW2_1_abc", OzoneHost: "auto"}
	for _, tc := range []struct {
		name, host, sid, arena, want string
	}{
		{"both", "auto", "4242", "1", ""},
		{"leading zeros", "auto", "0042", "0", ""},
		{"neither", "auto", "", "", ""},
		{"letters in the site", "auto", "42a", "", siteIDProblem},
		{"site too long", "auto", "12345678901", "", siteIDProblem},
		{"site out of range", "auto", "4294967296", "", siteIDProblem},
		{"arena too big", "auto", "", "256", arenaProblem},
		{"negative arena", "auto", "", "-1", arenaProblem},
		{"arena not a number", "auto", "", "one", arenaProblem},
		// The agent ignores them beside a fixed address, so a good pair is
		// harmless — but junk is still refused, not saved.
		{"fixed address, good pair", "192.0.2.10", "4242", "1", ""},
		{"fixed address, junk site", "192.0.2.10", "x", "", siteIDProblem},
		{"fixed address, junk arena", "192.0.2.10", "", "300", arenaProblem},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			in.OzoneHost, in.OzoneSID, in.OzoneArena = tc.host, tc.sid, tc.arena
			problems := in.validate()
			if tc.want == "" && len(problems) > 0 {
				t.Fatalf("rejected: %v", problems)
			}
			if tc.want != "" && !slicesContains(problems, tc.want) {
				t.Fatalf("problems = %v, want %q", problems, tc.want)
			}
		})
	}
}

func slicesContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// The form is decoded with DisallowUnknownFields, so a key the page sends that
// the server does not know rejects the whole save.
func TestSiteAndArenaRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.env")
	s, h := newTestServer(t, path)
	save := func(host, sid, arena string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{
			"CENTRAL_API_URL": "https://ow2.example/api/agent/ingest", "AGENT_TOKEN": "OW2_1_abc",
			"OZONE_WS_HOST": host, "OZONE_WS_PORT": "12113", "OZONE_SID": sid, "OZONE_ARENA": arena,
		})
		return do(t, h, http.MethodPost, "/api/config", s.key, string(body))
	}

	if w := save("auto", "4242", "1"); w.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
	}
	values, _ := decode(t, do(t, h, http.MethodGet, "/api/config", s.key, ""))["values"].(map[string]any)
	if values["OZONE_WS_HOST"] != "auto" || values["OZONE_SID"] != "4242" || values["OZONE_ARENA"] != "1" {
		t.Fatalf("read back %v, want auto, 4242 and 1", values)
	}

	// A fixed address with the pair left blank parks it behind a comment.
	if w := save("192.0.2.10", "", ""); w.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"OZONE_WS_HOST=192.0.2.10", "# OZONE_SID=4242", "# OZONE_ARENA=1"} {
		if !strings.Contains(string(saved), want) {
			t.Errorf("saved file is missing %q:\n%s", want, saved)
		}
	}
	values, _ = decode(t, do(t, h, http.MethodGet, "/api/config", s.key, ""))["values"].(map[string]any)
	if values["OZONE_SID"] != nil || values["OZONE_ARENA"] != nil {
		t.Errorf("a parked site or arena was read back as live: %v", values)
	}

	// And the decoder is still strict, so the pair above passing means
	// something.
	w := do(t, h, http.MethodPost, "/api/config", s.key, `{"CENTRAL_API_URL":"https://ow2.example/api/agent/ingest","AGENT_TOKEN":"OW2_1_abc","OZONE_WS_HOST":"auto","OZONE_SITE":"4242"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("an unknown key was accepted: %d", w.Code)
	}
}

// Every key the page sends must be one the server decodes, or every save fails.
func TestThePageSendsOnlyKeysTheServerKnows(t *testing.T) {
	known := map[string]bool{}
	st := reflect.TypeOf(settings{})
	for i := range st.NumField() {
		known[st.Field(i).Tag.Get("json")] = true
	}
	page, err := uiFS.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	text := string(page)
	block := func(name string) string {
		m := regexp.MustCompile(`const ` + name + ` = \{([^}]*)\}`).FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("the page has no %s", name)
		}
		return m[1]
	}
	key := regexp.MustCompile(`([A-Z][A-Z_]+):`)
	fields := map[string]bool{}
	for _, m := range key.FindAllStringSubmatch(block("FIELDS"), -1) {
		fields[m[1]] = true
		if !known[m[1]] {
			t.Errorf("the page sends %s, which the server does not decode", m[1])
		}
	}
	for _, m := range regexp.MustCompile(`\bs\.([A-Z][A-Z_]+) =`).FindAllStringSubmatch(text, -1) {
		if !known[m[1]] {
			t.Errorf("the page sets %s, which the server does not decode", m[1])
		}
	}
	defaults := block("DEFAULTS")
	for _, k := range []string{"OZONE_SID", "OZONE_ARENA"} {
		if !fields[k] {
			t.Errorf("%s is not one of the page's fields", k)
		}
		if !strings.Contains(defaults, k+`: ""`) {
			t.Errorf("%s has no empty default", k)
		}
	}
}

// "auto" is the word that turns discovery on, not a name to resolve: the
// address check lets it through, and the test button listens for the
// announcement instead of dialling it (TestGameServerTestWithAuto).
func TestGameServerProblemLetsAutoThrough(t *testing.T) {
	for _, host := range []string{"auto", "AUTO"} {
		if problem := gameServerProblem(host, "12113"); problem != "" {
			t.Errorf("gameServerProblem(%q) = %q, want no problem", host, problem)
		}
	}
}

// --- Finding the game server on the network ---

// heard is one announcement on a fake network: the server it names, and the
// address it was sent from.
type heard struct {
	ip, sender, sid string
	arena           int
}

// candidates runs announcements through the real parser and table, so a fake
// scan returns exactly what a real one would have.
func candidates(t *testing.T, hs ...heard) []ozonelib.Candidate {
	t.Helper()
	table := ozonelib.NewTable(time.Now)
	for _, h := range hs {
		p, err := ozonelib.Parse(ozonesim.LIBPacket(ozonesim.LIBAnnouncement{IP: h.ip, Arena: h.arena, SID: h.sid}))
		if err != nil {
			t.Fatalf("parse %+v: %v", h, err)
		}
		for range 2 {
			table.Observe(p, netip.MustParseAddr(h.sender), time.Now().UTC())
		}
	}
	return table.Candidates()
}

// fakeNet stands in for the network: a scan that returns what it was given at
// once, and a probe that answers from a table of modes and refuses any other
// address.
type fakeNet struct {
	cs      []ozonelib.Candidate
	offlink int64
	scanErr error
	modes   map[string]int
	delay   time.Duration

	mu     sync.Mutex
	scans  []time.Duration
	ports  []int
	probes []string // host:port, as asked

	inFlight, maxInFlight atomic.Int32
}

func (f *fakeNet) scan(_ context.Context, port int, d time.Duration) ([]ozonelib.Candidate, ozonelib.Counters, error) {
	f.mu.Lock()
	f.scans = append(f.scans, d)
	f.ports = append(f.ports, port)
	f.mu.Unlock()
	if f.scanErr != nil {
		return nil, ozonelib.Counters{}, f.scanErr
	}
	return f.cs, ozonelib.Counters{Packets: int64(2 * len(f.cs)), Offlink: f.offlink}, nil
}

func (f *fakeNet) probe(_ context.Context, host, port string, _ time.Duration) (int, error) {
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		m := f.maxInFlight.Load()
		if n <= m || f.maxInFlight.CompareAndSwap(m, n) {
			break
		}
	}
	f.mu.Lock()
	f.probes = append(f.probes, net.JoinHostPort(host, port))
	f.mu.Unlock()
	time.Sleep(f.delay)
	if mode, ok := f.modes[host]; ok {
		return mode, nil
	}
	return 0, errors.New("dial tcp " + net.JoinHostPort(host, port) + ": connect: connection refused")
}

func (f *fakeNet) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.probes...)
	sort.Strings(out)
	return out
}

func (f *fakeNet) scanned() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.scans)
}

const testLIBPort = 42106

func newDiscoverServer(t *testing.T, f *fakeNet) (*Server, http.Handler) {
	t.Helper()
	s, h := newTestServer(t, filepath.Join(t.TempDir(), "agent.env"))
	s.scan, s.probe, s.libPort = f.scan, f.probe, testLIBPort
	// The fixtures' TEST-NET addresses count as this computer's networks.
	s.onLink = func(netip.Addr) bool { return true }
	return s, h
}

func discover(t *testing.T, s *Server, h http.Handler, body string) map[string]any {
	t.Helper()
	w := do(t, h, http.MethodPost, "/api/discover", s.key, body)
	if w.Code != http.StatusOK {
		t.Fatalf("discover: status %d %s", w.Code, w.Body.String())
	}
	return decode(t, w)
}

func serverList(t *testing.T, got map[string]any) []map[string]any {
	t.Helper()
	list, ok := got["servers"].([]any)
	if !ok {
		t.Fatalf("servers = %#v, want a list", got["servers"])
	}
	out := make([]map[string]any, 0, len(list))
	for _, v := range list {
		out = append(out, v.(map[string]any))
	}
	return out
}

func TestNewListensOnTheConfiguredAnnouncementPort(t *testing.T) {
	s, _ := newTestServer(t, filepath.Join(t.TempDir(), "agent.env"))
	if s.libPort != ozonelib.DefaultPort || s.scan == nil || s.probe == nil {
		t.Fatalf("libPort = %d, scan set %v, probe set %v; want %d and both set", s.libPort, s.scan != nil, s.probe != nil, ozonelib.DefaultPort)
	}
	t.Setenv("OZONE_LIB_PORT", "42107")
	s, _ = newTestServer(t, filepath.Join(t.TempDir(), "agent.env"))
	if s.libPort != 42107 {
		t.Errorf("libPort = %d, want the configured 42107", s.libPort)
	}
}

func TestDiscoverWithNothingHeard(t *testing.T) {
	f := &fakeNet{offlink: 3}
	s, h := newDiscoverServer(t, f)
	got := discover(t, s, h, `{}`)

	if got["ok"] != false || got["listening"] != true || got["bind_error"] != "" {
		t.Errorf("ok = %v, listening = %v, bind_error = %v; want false, true, empty", got["ok"], got["listening"], got["bind_error"])
	}
	if n := len(serverList(t, got)); n != 0 {
		t.Errorf("%d servers listed with nothing heard", n)
	}
	advice, _ := got["advice"].(string)
	for _, want := range []string{"running", "subnet or VLAN", "firewall", "UDP 42106", "covers UDP 12106 only", "profile", "host networking", "3 announcements from outside"} {
		if !strings.Contains(advice, want) {
			t.Errorf("advice %q does not mention %q", advice, want)
		}
	}
	if counters, _ := got["counters"].(map[string]any); counters["offlink"] != float64(3) {
		t.Errorf("counters = %v, want offlink 3", got["counters"])
	}
	if a := f.asked(); len(a) != 0 {
		t.Errorf("probed %v with nothing heard", a)
	}
	if f.scans[0] != discoverWindow || f.ports[0] != testLIBPort {
		t.Errorf("scanned UDP %d for %s, want %d for %s", f.ports[0], f.scans[0], testLIBPort, discoverWindow)
	}
}

func TestDiscoverOneServer(t *testing.T) {
	f := &fakeNet{
		cs:    candidates(t, heard{"192.0.2.10", "192.0.2.10", "4242", 1}),
		modes: map[string]int{"192.0.2.10": 1},
	}
	s, h := newDiscoverServer(t, f)
	got := discover(t, s, h, `{"port":"12113"}`)

	if got["ok"] != true || got["advice"] != "" || got["port"] != "12113" || got["seconds"] != float64(11) {
		t.Errorf("ok = %v, advice = %q, port = %v, seconds = %v", got["ok"], got["advice"], got["port"], got["seconds"])
	}
	list := serverList(t, got)
	if len(list) != 1 {
		t.Fatalf("servers = %v, want one", list)
	}
	srv := list[0]
	for k, want := range map[string]any{
		"sid": float64(4242), "sid_raw": "4242", "arena": float64(1), "ip": "192.0.2.10", "sender": "192.0.2.10",
		"sender_ok": true, "local": false, "packets": float64(2), "ozone_time_text": "2024-08-06T12:24:59.129158",
		"answers": true, "mode": float64(1), "answer_error": "",
	} {
		if srv[k] != want {
			t.Errorf("%s = %#v, want %#v", k, srv[k], want)
		}
	}
	if at, _ := srv["received_at"].(string); at == "" {
		t.Error("received_at is missing")
	} else if _, err := time.Parse(time.RFC3339Nano, at); err != nil {
		t.Errorf("received_at %q: %v", at, err)
	}
	if _, ok := srv["sender_answers"]; ok {
		t.Error("a server sent from its own address had its sender asked as well")
	}
	if a := f.asked(); !reflect.DeepEqual(a, []string{"192.0.2.10:12113"}) {
		t.Errorf("probed %v, want only 192.0.2.10:12113", a)
	}
}

func TestDiscoverTwoServers(t *testing.T) {
	f := &fakeNet{
		cs: candidates(t,
			heard{"192.0.2.10", "192.0.2.10", "4242", 1},
			heard{"192.0.2.11", "192.0.2.11", "4242", 2},
			heard{"192.0.2.11", "192.0.2.11", "4242", 3}, // two arenas at one address: asked once
		),
		modes: map[string]int{"192.0.2.10": 1},
	}
	s, h := newDiscoverServer(t, f)
	got := discover(t, s, h, `{}`)

	list := serverList(t, got)
	if got["ok"] != true || len(list) != 3 {
		t.Fatalf("ok = %v, servers = %v; want true and three", got["ok"], list)
	}
	for i, want := range []struct {
		ip      string
		arena   float64
		answers bool
	}{{"192.0.2.10", 1, true}, {"192.0.2.11", 2, false}, {"192.0.2.11", 3, false}} {
		if list[i]["ip"] != want.ip || list[i]["arena"] != want.arena || list[i]["answers"] != want.answers {
			t.Errorf("server %d = %v, want %+v", i, list[i], want)
		}
	}
	if msg, _ := list[1]["answer_error"].(string); !strings.Contains(msg, "nothing is listening") {
		t.Errorf("answer_error = %q, want the operator's reading of a refused connection", msg)
	}
	if a := f.asked(); !reflect.DeepEqual(a, []string{"192.0.2.10:12113", "192.0.2.11:12113"}) {
		t.Errorf("probed %v, want each address once", a)
	}
}

// An announcement sent from an address other than the one it names is listed,
// not trusted, and both addresses are asked so the page can show which one is
// really the game server.
func TestDiscoverSenderMismatch(t *testing.T) {
	f := &fakeNet{
		cs:    candidates(t, heard{"192.0.2.10", "192.0.2.20", "4242", 1}),
		modes: map[string]int{"192.0.2.20": 1},
	}
	s, h := newDiscoverServer(t, f)
	got := discover(t, s, h, `{}`)

	list := serverList(t, got)
	if len(list) != 1 {
		t.Fatalf("servers = %v, want one", list)
	}
	srv := list[0]
	for k, want := range map[string]any{
		"ip": "192.0.2.10", "sender": "192.0.2.20", "sender_ok": false,
		"answers": false, "sender_answers": true, "sender_mode": float64(1), "sender_answer_error": "", "sender_local": false,
	} {
		if srv[k] != want {
			t.Errorf("%s = %#v, want %#v", k, srv[k], want)
		}
	}
	if got["ok"] != false || got["advice"] == "" {
		t.Errorf("ok = %v, advice = %q: an untrusted server alone is not a server the agent can follow", got["ok"], got["advice"])
	}
	if a := f.asked(); !reflect.DeepEqual(a, []string{"192.0.2.10:12113", "192.0.2.20:12113"}) {
		t.Errorf("probed %v, want both addresses", a)
	}
}

// The address a mismatched announcement names is whatever its sender wrote,
// so it is asked only if it is on this computer's networks; the sender, which
// always is, is still asked.
func TestDiscoverDoesNotAskAnOffLinkAddress(t *testing.T) {
	f := &fakeNet{
		cs:    candidates(t, heard{"203.0.113.9", "192.0.2.20", "4242", 1}),
		modes: map[string]int{"192.0.2.20": 1},
	}
	s, h := newDiscoverServer(t, f)
	s.onLink = func(a netip.Addr) bool { return a == netip.MustParseAddr("192.0.2.20") }
	srv := serverList(t, discover(t, s, h, `{}`))[0]
	if a := f.asked(); !reflect.DeepEqual(a, []string{"192.0.2.20:12113"}) {
		t.Errorf("probed %v, want only the sender", a)
	}
	if msg, _ := srv["answer_error"].(string); srv["answers"] != false || srv["mode"] != nil || !strings.Contains(msg, "not asked") {
		t.Errorf("answers = %v, mode = %v, answer_error = %q; want an address that was not asked", srv["answers"], srv["mode"], msg)
	}
	if srv["sender_answers"] != true {
		t.Errorf("sender_answers = %v, want the sender asked", srv["sender_answers"])
	}
}

// An announcement from another machine that names this computer's address is
// not a server on this computer.
func TestDiscoverDoesNotTakeAClaimedLocalAddressForThisComputer(t *testing.T) {
	f := &fakeNet{cs: candidates(t, heard{"127.0.0.1", "192.0.2.66", "4242", 1})}
	s, h := newDiscoverServer(t, f)
	srv := serverList(t, discover(t, s, h, `{}`))[0]
	if srv["local"] != false {
		t.Errorf("local = %v for an announcement sent from 192.0.2.66", srv["local"])
	}
}

func TestNoServerAdviceNamesTheInstalledRule(t *testing.T) {
	if a := noServerAdvice(ozonelib.DefaultPort); !strings.Contains(a, "UDP 12106 (the installer adds the rule)") {
		t.Errorf("advice for the default port = %q", a)
	}
	if a := noServerAdvice(12107); strings.Contains(a, "installer adds") || !strings.Contains(a, "UDP 12107 needs a rule of its own") {
		t.Errorf("advice for UDP 12107 = %q", a)
	}
}

func TestDiscoverMarksAServerOnThisComputer(t *testing.T) {
	f := &fakeNet{cs: candidates(t,
		heard{"127.0.0.1", "127.0.0.1", "4242", 1},
		heard{"192.0.2.10", "192.0.2.10", "4242", 2},
	)}
	s, h := newDiscoverServer(t, f)
	list := serverList(t, discover(t, s, h, `{}`))
	if len(list) != 2 {
		t.Fatalf("servers = %v, want two", list)
	}
	if list[0]["ip"] != "127.0.0.1" || list[0]["local"] != true {
		t.Errorf("server 0 = %v, want 127.0.0.1 marked local", list[0])
	}
	if list[1]["local"] != false {
		t.Errorf("server 1 = %v, want it not local", list[1])
	}
}

func TestDiscoverReportsABindFailure(t *testing.T) {
	f := &fakeNet{scanErr: errors.New("listen udp4 :42106: bind: address already in use")}
	s, h := newDiscoverServer(t, f)
	got := discover(t, s, h, `{}`)

	if got["ok"] != false || got["listening"] != false {
		t.Errorf("ok = %v, listening = %v; want both false", got["ok"], got["listening"])
	}
	if e, _ := got["bind_error"].(string); !strings.Contains(e, "address already in use") {
		t.Errorf("bind_error = %q", e)
	}
	if msg, _ := got["message"].(string); !strings.Contains(msg, "UDP 42106") {
		t.Errorf("message = %q, want it to name the port", msg)
	}
	if got["advice"] == "" {
		t.Error("no advice with a bind failure")
	}
	if a := f.asked(); len(a) != 0 {
		t.Errorf("probed %v without listening", a)
	}
}

func TestDiscoverListensForAsLongAsAsked(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status int
		want   time.Duration
	}{
		{"", http.StatusOK, discoverWindow},
		{`{}`, http.StatusOK, discoverWindow},
		{`{"seconds":1}`, http.StatusOK, time.Second},
		{`{"seconds":30}`, http.StatusOK, 30 * time.Second},
		{`{"seconds":0}`, http.StatusBadRequest, 0},
		{`{"seconds":31}`, http.StatusBadRequest, 0},
		{`{"seconds":-5}`, http.StatusBadRequest, 0},
		{`{"seconds":11,"extra":1}`, http.StatusBadRequest, 0},
	} {
		t.Run(tc.body, func(t *testing.T) {
			f := &fakeNet{}
			s, h := newDiscoverServer(t, f)
			w := do(t, h, http.MethodPost, "/api/discover", s.key, tc.body)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tc.status, w.Body.String())
			}
			if tc.status != http.StatusOK {
				if f.scanned() != 0 {
					t.Error("a refused request still listened")
				}
				return
			}
			if f.scans[0] != tc.want {
				t.Errorf("listened for %s, want %s", f.scans[0], tc.want)
			}
		})
	}
}

// Discovery asks the port the agent polls, never the print server: a stray
// request to it during a game is exactly what the agent exists to prevent.
func TestDiscoverNeverProbesThePrintServer(t *testing.T) {
	t.Setenv("OZONE_RESULTS_PORT", "12999")
	cs := candidates(t, heard{"192.0.2.10", "192.0.2.10", "4242", 1})

	f := &fakeNet{cs: cs}
	s, h := newDiscoverServer(t, f)
	discover(t, s, h, `{"port":"12200"}`)
	if a := f.asked(); !reflect.DeepEqual(a, []string{"192.0.2.10:12200"}) {
		t.Errorf("probed %v, want the requested port", a)
	}

	for _, port := range []string{"12123", "012123", "12999"} {
		t.Run(port, func(t *testing.T) {
			f := &fakeNet{cs: cs}
			s, h := newDiscoverServer(t, f)
			got := discover(t, s, h, `{"port":"`+port+`"}`)
			if msg, _ := got["message"].(string); got["ok"] != false || !strings.Contains(msg, "print server") {
				t.Errorf("ok = %v, message = %q; want a refusal naming the print server", got["ok"], msg)
			}
			if f.scanned() != 0 || len(f.asked()) != 0 {
				t.Errorf("listened %d times and probed %v for a refused port", f.scanned(), f.asked())
			}
		})
	}
}

func TestDiscoverProbesAtMostFourAtOnce(t *testing.T) {
	var hs []heard
	for i := 1; i <= 10; i++ {
		ip := "192.0.2." + strconv.Itoa(i)
		hs = append(hs, heard{ip, ip, "4242", i})
	}
	f := &fakeNet{cs: candidates(t, hs...), delay: 30 * time.Millisecond}
	s, h := newDiscoverServer(t, f)
	discover(t, s, h, `{}`)

	if n := len(f.asked()); n != 10 {
		t.Fatalf("probed %d addresses, want 10", n)
	}
	if m := f.maxInFlight.Load(); m > 4 || m < 2 {
		t.Errorf("%d probes ran at once, want between 2 and 4", m)
	}
}

func TestDiscoverIsGuarded(t *testing.T) {
	f := &fakeNet{cs: candidates(t, heard{"192.0.2.10", "192.0.2.10", "4242", 1})}
	s, h := newDiscoverServer(t, f)

	if got := do(t, h, http.MethodPost, "/api/discover", "", `{}`).Code; got != http.StatusForbidden {
		t.Errorf("no key: status = %d, want 403", got)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/discover", strings.NewReader(`{}`))
	r.Host = "rebind.example.com"
	r.Header.Set("X-Setup-Key", s.key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("a hostname: status = %d, want 404", w.Code)
	}
	if got := do(t, h, http.MethodGet, "/api/discover", s.key, "").Code; got != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", got)
	}
	if f.scanned() != 0 || len(f.asked()) != 0 {
		t.Errorf("a refused request listened %d times and probed %v", f.scanned(), f.asked())
	}
}

// The real listener, end to end: an announcement sent to the port is heard,
// listed and asked about.
func TestDiscoverHearsARealAnnouncement(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()

	f := &fakeNet{modes: map[string]int{"127.0.0.1": 2}}
	s, h := newTestServer(t, filepath.Join(t.TempDir(), "agent.env"))
	s.probe, s.libPort = f.probe, port // the real scan

	sender := &ozonesim.LIBSender{Target: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), Source: "127.0.0.1"}
	sender.Start(ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 1, SID: "4242"}, 50*time.Millisecond)
	defer sender.Stop()

	got := discover(t, s, h, `{"seconds":1}`)
	list := serverList(t, got)
	if got["ok"] != true || len(list) != 1 {
		t.Fatalf("ok = %v, servers = %v; want the announcement heard", got["ok"], list)
	}
	if list[0]["ip"] != "127.0.0.1" || list[0]["local"] != true || list[0]["mode"] != float64(2) {
		t.Errorf("server = %v", list[0])
	}
}

// --- The game server test with auto ---

func TestGameServerTestWithAuto(t *testing.T) {
	one := candidates(t, heard{"192.0.2.10", "192.0.2.10", "4242", 1})
	twoArenas := candidates(t, heard{"192.0.2.10", "192.0.2.10", "4242", 1}, heard{"192.0.2.11", "192.0.2.11", "4242", 2})
	cases := []struct {
		name       string
		cs         []ozonelib.Candidate
		scanErr    error
		modes      map[string]int
		fields     map[string]string
		ok         bool
		expect     string
		wantProbes []string
		wantScan   bool
	}{
		{
			name: "found and answered", cs: one, modes: map[string]int{"192.0.2.10": 1},
			fields: map[string]string{"host": "auto", "port": "12113", "sid": "4242", "arena": "1"},
			ok:     true, expect: "Found O-Zone at 192.0.2.10 (site 4242, arena 1) and it answered (mode 1)",
			wantProbes: []string{"192.0.2.10:12113"}, wantScan: true,
		},
		{
			name: "found, no site or arena given", cs: one, modes: map[string]int{"192.0.2.10": 1},
			fields: map[string]string{"host": " AUTO "},
			ok:     true, expect: "Fill in its site and arena",
			wantProbes: []string{"192.0.2.10:12113"}, wantScan: true,
		},
		{
			name: "found but did not answer", cs: one,
			fields:     map[string]string{"host": "auto"},
			expect:     "Found O-Zone at 192.0.2.10 (site 4242, arena 1), but it did not answer on port 12113",
			wantProbes: []string{"192.0.2.10:12113"}, wantScan: true,
		},
		{
			name: "two arenas", cs: twoArenas, modes: map[string]int{"192.0.2.10": 1, "192.0.2.11": 1},
			fields: map[string]string{"host": "auto"},
			expect: "Heard 2 servers — choose an arena", wantScan: true,
		},
		{
			name: "two sites", modes: map[string]int{"192.0.2.10": 1, "192.0.2.11": 1},
			cs:     candidates(t, heard{"192.0.2.10", "192.0.2.10", "4242", 1}, heard{"192.0.2.11", "192.0.2.11", "4343", 1}),
			fields: map[string]string{"host": "auto"},
			expect: "Heard 2 servers — choose a site", wantScan: true,
		},
		{
			name: "the arena chooses", cs: twoArenas, modes: map[string]int{"192.0.2.10": 1, "192.0.2.11": 5},
			fields: map[string]string{"host": "auto", "arena": "2"},
			ok:     true, expect: "Found O-Zone at 192.0.2.11 (site 4242, arena 2) and it answered (mode 5)",
			wantProbes: []string{"192.0.2.11:12113"}, wantScan: true,
		},
		{
			name: "one server at two addresses", modes: map[string]int{"192.0.2.10": 1, "192.0.2.11": 1},
			cs:     candidates(t, heard{"192.0.2.10", "192.0.2.10", "4242", 1}, heard{"192.0.2.11", "192.0.2.11", "4242", 1}),
			fields: map[string]string{"host": "auto"},
			expect: "announced from 2 addresses", wantScan: true,
		},
		{
			name:   "nothing heard",
			fields: map[string]string{"host": "auto"},
			expect: "Heard no game server announcement on UDP 42106", wantScan: true,
		},
		{
			name: "nothing for that site", cs: one, modes: map[string]int{"192.0.2.10": 1},
			fields: map[string]string{"host": "auto", "sid": "4343"},
			expect: "nothing for site 4343", wantScan: true,
		},
		{
			name: "only an untrusted announcement", modes: map[string]int{"192.0.2.10": 1, "192.0.2.20": 1},
			cs:     candidates(t, heard{"192.0.2.10", "192.0.2.20", "4242", 1}),
			fields: map[string]string{"host": "auto"},
			expect: "never trusts", wantScan: true,
		},
		{
			name: "cannot listen", scanErr: errors.New("bind: address already in use"),
			fields: map[string]string{"host": "auto"},
			expect: "Could not listen for the game server's announcement on UDP 42106", wantScan: true,
		},
		{
			name: "a malformed site", cs: one,
			fields: map[string]string{"host": "auto", "sid": "42x"},
			expect: siteIDProblem,
		},
		{
			name: "a malformed arena", cs: one,
			fields: map[string]string{"host": "auto", "arena": "256"},
			expect: arenaProblem,
		},
		{
			name: "the print server's port", cs: one, modes: map[string]int{"192.0.2.10": 1},
			fields: map[string]string{"host": "auto", "port": "12123"},
			expect: "print server",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeNet{cs: c.cs, scanErr: c.scanErr, modes: c.modes}
			s, h := newDiscoverServer(t, f)
			body, _ := json.Marshal(c.fields)
			got := decode(t, do(t, h, http.MethodPost, "/api/test/gameserver", s.key, string(body)))

			msg, _ := got["message"].(string)
			if got["ok"] != c.ok {
				t.Errorf("ok = %v, want %v (%s)", got["ok"], c.ok, msg)
			}
			if !strings.Contains(msg, c.expect) {
				t.Errorf("message = %q, want it to mention %q", msg, c.expect)
			}
			if a := f.asked(); !reflect.DeepEqual(a, c.wantProbes) && (len(a) != 0 || len(c.wantProbes) != 0) {
				t.Errorf("probed %v, want %v", a, c.wantProbes)
			}
			if (f.scanned() == 1) != c.wantScan {
				t.Errorf("listened %d times, want listening %v", f.scanned(), c.wantScan)
			}
			// "auto" never reaches a resolver or a dial: had it been dialled, the
			// message would be the resolver's.
			for _, a := range f.asked() {
				if strings.HasPrefix(strings.ToLower(a), "auto") {
					t.Errorf("probed %q", a)
				}
			}
			if strings.Contains(msg, "does not resolve") || strings.Contains(strings.ToLower(msg), "auto:") {
				t.Errorf("message = %q: auto was treated as an address", msg)
			}
		})
	}
}

// Testing a fixed address never sends anything to the print server's port.
func TestGameServerTestWithAFixedAddressRefusesThePrintServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- struct{}{}
			c.Close()
		}
	}()
	_, results, _ := net.SplitHostPort(ln.Addr().String())
	t.Setenv("OZONE_RESULTS_PORT", results)

	f := &fakeNet{}
	s, h := newDiscoverServer(t, f)
	for _, port := range []string{"12123", results} {
		got := decode(t, do(t, h, http.MethodPost, "/api/test/gameserver", s.key, `{"host":"127.0.0.1","port":"`+port+`"}`))
		if msg, _ := got["message"].(string); got["ok"] != false || !strings.Contains(msg, "print server") {
			t.Errorf("port %s: ok = %v, message = %q; want a refusal naming the print server", port, got["ok"], msg)
		}
	}
	select {
	case <-accepted:
		t.Error("the test connected to the print server's port")
	case <-time.After(100 * time.Millisecond):
	}
}

// A fixed address is tested as it always was: dialled directly, with no
// listening and no discovery probe.
func TestGameServerTestWithAFixedAddressDoesNotListen(t *testing.T) {
	f := &fakeNet{cs: candidates(t, heard{"192.0.2.10", "192.0.2.10", "4242", 1})}
	s, h := newDiscoverServer(t, f)
	got := decode(t, do(t, h, http.MethodPost, "/api/test/gameserver", s.key, `{"host":"127.0.0.1","port":"1","sid":"4242","arena":"1"}`))
	if msg, _ := got["message"].(string); got["ok"] != false || !strings.Contains(msg, "Could not reach the game server at 127.0.0.1:1") {
		t.Errorf("ok = %v, message = %q", got["ok"], msg)
	}
	if f.scanned() != 0 || len(f.asked()) != 0 {
		t.Errorf("a fixed address listened %d times and probed %v", f.scanned(), f.asked())
	}
}
