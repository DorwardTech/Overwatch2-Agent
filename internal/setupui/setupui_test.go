package setupui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
