// Package setupui is the agent's configuration page: a small web server, bound
// to the loopback address only, that an operator uses instead of editing the
// configuration file by hand.
//
// The venues that run this agent are laser-tag centres, and the person
// installing it is whoever is on shift. Asking them to open a text file as
// administrator, keep KEY=VALUE syntax intact, and then read a log file to
// find out whether the token was right is asking for a support call. The page
// gives them a form, tests the two connections that actually matter, writes
// the file, and starts the service.
//
// It is deliberately not the venue control panel: it binds the loopback
// address, it is reachable only by the person who launched it (a one-time key
// in the address the browser is opened at), and it exits when they are done.
package setupui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"overwatch/agent/internal/config"
	"overwatch/agent/internal/ozone"
	"overwatch/agent/internal/push"
)

//go:embed ui.html
var uiFS embed.FS

// IdleTimeout closes the page down when nobody is using it any more, so a
// forgotten browser tab does not leave a configuration server listening.
const IdleTimeout = 30 * time.Minute

// Service is the part of the service control the page can drive. It is
// implemented by the platform-specific code in the command, and left
// unimplemented where there is no service manager to speak to.
type Service interface {
	// Supported reports whether this platform has a service to manage.
	Supported() bool
	// Installed reports whether the service exists yet.
	Installed() (bool, error)
	// State is a word for the operator: "running", "stopped", …
	State() (string, error)
	Install() error
	Start() error
	Stop() error
	Restart() error
}

// Server is the configuration page.
type Server struct {
	configPath string
	dataDir    string
	logPath    string
	service    Service
	key        string // one-time key, carried in the address the browser opens

	mu       sync.Mutex
	lastSeen time.Time

	ln   net.Listener
	http *http.Server
	tmpl *template.Template
}

// New prepares the page. It does not listen until Serve is called.
func New(configPath, dataDir, logPath string, svc Service) (*Server, error) {
	key, err := newKey()
	if err != nil {
		return nil, err
	}
	raw, err := fs.ReadFile(uiFS, "ui.html")
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("ui").Parse(string(raw))
	if err != nil {
		return nil, err
	}
	if svc == nil {
		svc = unsupportedService{}
	}
	return &Server{
		configPath: configPath,
		dataDir:    dataDir,
		logPath:    logPath,
		service:    svc,
		key:        key,
		lastSeen:   time.Now(),
		tmpl:       tmpl,
	}, nil
}

// Listen binds the loopback address and returns the URL to open. Binding
// happens before the browser is launched so the page is already answering
// when it arrives — and so a port clash is an error the operator sees, rather
// than a browser tab that cannot connect.
func (s *Server) Listen() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("open the setup page: %w", err)
	}
	s.ln = ln
	return fmt.Sprintf("http://%s/?key=%s", ln.Addr().String(), s.key), nil
}

// Serve runs until the operator finishes, the context is cancelled, or the
// page goes unused for IdleTimeout.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		if _, err := s.Listen(); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.http = &http.Server{
		Handler:           s.handler(cancel),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go s.watchIdle(ctx, cancel)

	errc := make(chan error, 1)
	go func() { errc <- s.http.Serve(s.ln) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, done := context.WithTimeout(context.Background(), 3*time.Second)
		defer done()
		_ = s.http.Shutdown(shutdown)
		return nil
	}
}

// handler builds the routes. done is called when the operator says they have
// finished, which is what stops the server.
func (s *Server) handler(done context.CancelFunc) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handlePage)
	mux.HandleFunc("/api/config", s.guard(s.handleConfig))
	mux.HandleFunc("/api/test/central", s.guard(s.handleTestCentral))
	mux.HandleFunc("/api/test/gameserver", s.guard(s.handleTestGameServer))
	mux.HandleFunc("/api/service", s.guard(s.handleService))
	mux.HandleFunc("/api/log", s.guard(s.handleLog))
	mux.HandleFunc("/api/finish", s.guard(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		done()
	}))
	return mux
}

func (s *Server) watchIdle(ctx context.Context, cancel context.CancelFunc) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			idle := time.Since(s.lastSeen)
			s.mu.Unlock()
			if idle > IdleTimeout {
				log.Printf("[setup] closing the setup page after %s unused", IdleTimeout)
				cancel()
				return
			}
		}
	}
}

func (s *Server) touch() {
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

// guard is the page's whole access control, and it is worth being plain about
// what each part stops.
//
//   - The listener is bound to 127.0.0.1, so nothing off this machine can
//     reach it at all.
//   - The key, generated per run and carried in the address the browser is
//     opened at, stops another *user* of the same machine driving the page.
//   - Requiring the key in a header, not a cookie or a query parameter, is
//     what stops a web page the operator happens to have open from posting to
//     the port behind their back: a form post cannot set a header, and the
//     cross-origin fetch that could is refused by the browser because nothing
//     here answers a preflight or sends an allow-origin header.
//   - Checking Host closes DNS rebinding, where a hostile name resolves to
//     127.0.0.1 so the browser thinks its own origin is the one being called.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		key := r.Header.Get("X-Setup-Key")
		if subtle.ConstantTimeCompare([]byte(key), []byte(s.key)) != 1 {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "This setup page has expired. Close the tab and run the setup command again."})
			return
		}
		s.touch()
		next(w, r)
	}
}

// loopbackHost reports whether the request was addressed to this machine's own
// loopback address rather than to a name that merely resolves to it.
func loopbackHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !loopbackHost(r.Host) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("key")), []byte(s.key)) != 1 {
		http.Error(w, "This setup page has expired. Close the tab and run the setup command again.", http.StatusForbidden)
		return
	}
	s.touch()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The page is only ever served here; nothing it needs comes from anywhere
	// else, so say so.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; form-action 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if err := s.tmpl.Execute(w, map[string]any{
		"Key":        s.key,
		"ConfigPath": s.configPath,
		"LogPath":    s.logPath,
		"DataDir":    s.dataDir,
		"Service":    s.service.Supported(),
	}); err != nil {
		log.Printf("[setup] render: %v", err)
	}
}

// settings are the fields the page manages. Everything else in the file is
// left alone.
type settings struct {
	CentralURL      string `json:"CENTRAL_API_URL"`
	Token           string `json:"AGENT_TOKEN"`
	OzoneHost       string `json:"OZONE_WS_HOST"`
	OzonePort       string `json:"OZONE_WS_PORT"`
	ResultsEnabled  string `json:"ENABLE_GAME_RESULTS"`
	CacheEnabled    string `json:"ENABLE_CACHE"`
	ProxyEnabled    string `json:"ENABLE_PROXY"`
	MsgBusEnabled   string `json:"ENABLE_MSG_BUS"`
	ProxyListenAddr string `json:"PROXY_LISTEN_ADDR"`
	AdminAddr       string `json:"ADMIN_API_ADDR"`
	AdminToken      string `json:"ADMIN_API_TOKEN"`
	PollInterval    string `json:"POLL_INTERVAL"`
	IdlePoll        string `json:"IDLE_POLL_INTERVAL"`
	SlowPoll        string `json:"SLOW_POLL_INTERVAL"`
	HealthAddr      string `json:"HEALTH_ADDR"`
}

func (s settings) vars() []config.EnvVar {
	return []config.EnvVar{
		{Key: "CENTRAL_API_URL", Value: s.CentralURL},
		{Key: "AGENT_TOKEN", Value: s.Token},
		{Key: "OZONE_WS_HOST", Value: s.OzoneHost},
		{Key: "OZONE_WS_PORT", Value: s.OzonePort},
		{Key: "ENABLE_GAME_RESULTS", Value: s.ResultsEnabled},
		{Key: "ENABLE_CACHE", Value: s.CacheEnabled},
		{Key: "ENABLE_PROXY", Value: s.ProxyEnabled},
		{Key: "ENABLE_MSG_BUS", Value: s.MsgBusEnabled},
		{Key: "PROXY_LISTEN_ADDR", Value: s.ProxyListenAddr},
		{Key: "ADMIN_API_ADDR", Value: s.AdminAddr},
		{Key: "ADMIN_API_TOKEN", Value: s.AdminToken},
		{Key: "POLL_INTERVAL", Value: s.PollInterval},
		{Key: "IDLE_POLL_INTERVAL", Value: s.IdlePoll},
		{Key: "SLOW_POLL_INTERVAL", Value: s.SlowPoll},
		{Key: "HEALTH_ADDR", Value: s.HealthAddr},
	}
}

// validate rejects what the agent would refuse or misread later, in the words
// the operator needs to fix it.
func (s settings) validate() []string {
	var problems []string
	switch {
	case strings.TrimSpace(s.CentralURL) == "":
		problems = append(problems, "The Overwatch address is required.")
	case !strings.HasPrefix(s.CentralURL, "http://") && !strings.HasPrefix(s.CentralURL, "https://"):
		problems = append(problems, "The Overwatch address must start with https://")
	}
	if strings.TrimSpace(s.Token) == "" {
		problems = append(problems, "The site token is required — copy it from the Sites screen in Overwatch.")
	}
	if strings.ContainsAny(s.Token, "\"' \t") {
		problems = append(problems, "The site token contains a space or a quote mark — copy it again, without the surrounding text.")
	}
	if strings.TrimSpace(s.OzoneHost) == "" {
		problems = append(problems, "The game server address is required (127.0.0.1 if the agent runs on that machine).")
	}
	for _, f := range []struct{ name, value string }{
		{"The game server port", s.OzonePort},
		{"The fast poll interval", s.PollInterval},
		{"The idle poll interval", s.IdlePoll},
		{"The slow poll interval", s.SlowPoll},
	} {
		if f.value != "" && !allDigits(f.value) {
			problems = append(problems, f.name+" must be a number.")
		}
	}
	for _, f := range []struct{ name, value string }{
		{"The scoresheet listener address", s.ProxyListenAddr},
		{"The control panel address", s.AdminAddr},
		{"The health address", s.HealthAddr},
	} {
		if f.value == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(f.value); err != nil {
			problems = append(problems, f.name+` must look like "0.0.0.0:12124" — an address and a port.`)
		}
	}
	if s.AdminAddr != "" && s.AdminToken == "" {
		problems = append(problems, "The control panel needs a password as well as an address.")
	}
	return problems
}

func allDigits(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// handleConfig reads the current settings, or writes new ones.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.readConfig(w)
	case http.MethodPost:
		s.writeConfig(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) readConfig(w http.ResponseWriter) {
	values := map[string]string{}
	raw, err := os.ReadFile(s.configPath)
	switch {
	case err == nil:
		vars, err := config.ParseEnv(strings.NewReader(string(raw)))
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"values":  values,
				"warning": fmt.Sprintf("%s could not be read (%v). Saving will rewrite it.", s.configPath, err),
			})
			return
		}
		for _, v := range vars {
			values[v.Key] = v.Value
		}
	case errors.Is(err, fs.ErrNotExist):
		// A first run: the form opens on its defaults.
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"values": values})
}

func (s *Server) writeConfig(w http.ResponseWriter, r *http.Request) {
	var in settings
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if problems := in.validate(); len(problems) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"problems": problems})
		return
	}
	if err := s.save(in); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": s.configPath})
}

// save rewrites the configuration file, keeping everything it does not manage
// and replacing it atomically so a failure part-way cannot leave the agent
// with half a file.
func (s *Server) save(in settings) error {
	existing, err := os.ReadFile(s.configPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", s.configPath, err)
	}
	merged := config.MergeEnv(string(existing), in.vars())

	dir := filepath.Dir(s.configPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".agent.env-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", s.configPath, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(merged); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", s.configPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", s.configPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", s.configPath, err)
	}
	// The file inherits the data directory's permissions, which the installer
	// restricted; 0600 is what keeps the token private everywhere else.
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("set permissions on %s: %w", s.configPath, err)
	}
	if err := os.Rename(tmp.Name(), s.configPath); err != nil {
		return fmt.Errorf("replace %s: %w", s.configPath, err)
	}
	return nil
}

// handleTestCentral proves the address, the network and the token in one go,
// and says which of them is wrong when it fails.
func (s *Server) handleTestCentral(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if strings.TrimSpace(in.URL) == "" || strings.TrimSpace(in.Token) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "Fill in the Overwatch address and the site token first."})
		return
	}

	err := push.New(strings.TrimSpace(in.URL), strings.TrimSpace(in.Token)).Probe()
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Connected to Overwatch and the site token was accepted."})
		return
	}
	var httpErr *push.HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "Overwatch answered, but rejected this site token. Generate a fresh one on the Sites screen and paste it again."})
		case http.StatusNotFound:
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "Reached the server, but there is nothing at that address. Check the Overwatch address — it should end in /api/agent/ingest."})
		case http.StatusTooManyRequests:
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "Overwatch is rate-limiting this site. Wait a minute and test again."})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": fmt.Sprintf("Overwatch answered with an error (HTTP %d). If it persists, tell the Overwatch administrator.", httpErr.StatusCode)})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "Could not reach Overwatch: " + networkAdvice(err)})
}

// handleTestGameServer connects to the game server's read-only telemetry API —
// the same port the agent polls. It never touches the print server: that is
// the one thing that must not be disturbed while a game is on.
func (s *Server) handleTestGameServer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Host string `json:"host"`
		Port string `json:"port"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	host, port := strings.TrimSpace(in.Host), strings.TrimSpace(in.Port)
	if host == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "Fill in the game server address first."})
		return
	}
	if port == "" {
		port = "12113"
	}

	client, err := ozone.Dial(host, port)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": fmt.Sprintf("Could not reach the game server at %s:%s: %s", host, port, networkAdvice(err))})
		return
	}
	defer client.Close()

	resp, err := client.Command("GETSERVERSTATE")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "Connected to the game server, but it did not answer: " + err.Error()})
		return
	}
	msg := "Connected to the game server."
	if mode, ok := resp["SERVERMODE"]; ok {
		msg = fmt.Sprintf("Connected to the game server (it reports mode %v).", mode)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
}

// networkAdvice turns a dial error into something an operator can act on.
func networkAdvice(err error) string {
	text := err.Error()
	switch {
	case strings.Contains(text, "no such host"):
		return "that name does not resolve. Check the address for a typo."
	case strings.Contains(text, "refused"):
		return "nothing is listening on that port. Check the address and that the other end is running."
	case strings.Contains(text, "timeout") || strings.Contains(text, "timed out") || strings.Contains(text, "deadline"):
		return "it did not answer in time. Check the address, the network, and any firewall in between."
	case strings.Contains(text, "certificate"):
		return "the secure connection could not be verified. Check the address is the real Overwatch server."
	default:
		return text
	}
}

func (s *Server) handleService(w http.ResponseWriter, r *http.Request) {
	if !s.service.Supported() {
		writeJSON(w, http.StatusOK, map[string]any{"supported": false})
		return
	}
	if r.Method == http.MethodGet {
		s.writeServiceState(w, "")
		return
	}
	var in struct {
		Action string `json:"action"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	var err error
	switch in.Action {
	case "install":
		err = s.service.Install()
	case "start":
		err = s.service.Start()
	case "stop":
		err = s.service.Stop()
	case "restart":
		err = s.service.Restart()
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown action " + in.Action})
		return
	}
	if err != nil {
		s.writeServiceState(w, err.Error())
		return
	}
	s.writeServiceState(w, "")
}

func (s *Server) writeServiceState(w http.ResponseWriter, failure string) {
	installed, err := s.service.Installed()
	if err != nil && failure == "" {
		failure = err.Error()
	}
	state := "not installed"
	if installed {
		if st, err := s.service.State(); err == nil {
			state = st
		} else if failure == "" {
			failure = err.Error()
		}
	}
	body := map[string]any{"supported": true, "installed": installed, "state": state}
	if failure != "" {
		body["error"] = failure
	}
	writeJSON(w, http.StatusOK, body)
}

// handleLog returns the tail of the agent's log, so the operator can watch it
// start without opening a terminal.
func (s *Server) handleLog(w http.ResponseWriter, _ *http.Request) {
	const maxTail = 16 << 10
	if s.logPath == "" {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}, "path": ""})
		return
	}
	f, err := os.Open(s.logPath)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}, "path": s.logPath, "note": "The log file does not exist yet."})
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}, "path": s.logPath})
		return
	}
	start := info.Size() - maxTail
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}, "path": s.logPath})
		return
	}
	buf, err := io.ReadAll(io.LimitReader(f, maxTail+1))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}, "path": s.logPath})
		return
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\r\n"), "\n")
	if start > 0 && len(lines) > 1 {
		lines = lines[1:] // drop the half line the seek landed in
	}
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "path": s.logPath})
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("could not read the form: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func newKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// unsupportedService stands in where there is no service manager: the page
// still writes the configuration, it just cannot start anything.
type unsupportedService struct{}

func (unsupportedService) Supported() bool          { return false }
func (unsupportedService) Installed() (bool, error) { return false, nil }
func (unsupportedService) State() (string, error)   { return "", nil }
func (unsupportedService) Install() error           { return errors.New("no service manager on this platform") }
func (unsupportedService) Start() error             { return errors.New("no service manager on this platform") }
func (unsupportedService) Stop() error              { return errors.New("no service manager on this platform") }
func (unsupportedService) Restart() error           { return errors.New("no service manager on this platform") }
