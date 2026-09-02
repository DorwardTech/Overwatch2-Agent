//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"overwatch/agent/internal/platform"
	"overwatch/agent/internal/version"
)

const (
	serviceName        = "OverwatchAgent"
	serviceDisplayName = "Overwatch Site Agent"
	serviceDescription = "Connects this venue's laser-tag game server to Overwatch: polls it locally and pushes telemetry, game results and alerts to the central Overwatch server."

	// serviceAccount is the least-privileged built-in account that can still
	// open outbound connections and listen on the LAN. It is not an
	// administrator, and it can read and write only what the installer grants
	// it: the data directory.
	serviceAccount = `NT AUTHORITY\LocalService`

	// serviceSIDName is the per-service identity Windows derives from the
	// service name once the service declares a service SID. Granting the data
	// directory to this rather than to LocalService keeps the site token and
	// the cached game data away from every *other* service that also runs as
	// LocalService — which is most of them.
	serviceSIDName = `NT SERVICE\` + serviceName

	// exitRestartRequested is reported when the agent ends on its own while
	// the service was not asked to stop — a reboot_agent command from central.
	// It is reported as a failure so the recovery actions restart the service,
	// exactly as the container runtime restarts a container that exits.
	exitRestartRequested = 3

	stopTimeout = 30 * time.Second
)

// Well-known SIDs, so the ACL is the same on a machine whose group names are
// localised.
const (
	sidAdministrators = "*S-1-5-32-544"
	sidSystem         = "*S-1-5-18"
	sidLocalService   = "*S-1-5-19"
)

func isWindowsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// runAsService hands the process to the service control manager. svc.Run
// returns once the service has stopped; the handler's exit code becomes the
// process's.
func runAsService(opts options) int {
	h := &serviceHandler{opts: opts, exitCode: exitFailure}
	if err := svc.Run(serviceName, h); err != nil {
		reportServiceError(fmt.Sprintf("service control manager: %v", err))
		return exitFailure
	}
	return h.exitCode
}

type serviceHandler struct {
	opts     options
	exitCode int
}

// Execute is the service body. The configuration is loaded before the service
// reports itself running, so a missing token or an unreadable configuration
// file is a failed start — visible to `start`, to services.msc and in the
// event log — rather than a service that reports success and stops a moment
// later. Once that succeeds the agent runs in its own goroutine while this
// loop answers the control manager; the agent retries its connections for as
// long as it takes, so nothing else is worth waiting for.
func (h *serviceHandler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	const startWaitHint = 20 * time.Second
	s <- svc.Status{State: svc.StartPending, WaitHint: uint32(startWaitHint / time.Millisecond)}

	cfg, closeLog, err := prepareAgent(h.opts, true)
	defer closeLog()
	if err != nil {
		reportServiceError(fmt.Sprintf("%s could not start: %v", serviceDisplayName, err))
		h.exitCode = exitFailure
		return true, exitFailure
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan int, 1)
	go func() { done <- runLoaded(ctx, cfg) }()

	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	reportServiceInfo(fmt.Sprintf("%s %s started", serviceDisplayName, cfg.Version))

	for {
		select {
		case code := <-done:
			// The agent ended without being asked to. Report a failure so the
			// recovery actions restart the service; a clean stop would leave
			// it down until somebody noticed the site offline.
			s <- svc.Status{State: svc.StopPending}
			if code == exitOK {
				code = exitRestartRequested
				reportServiceInfo("agent asked to be restarted; the service control manager will restart it")
			} else {
				reportServiceError(fmt.Sprintf("agent exited with code %d — see the log file for the reason", code))
			}
			h.exitCode = code
			return true, uint32(code)
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				cancel()
				h.drain(done, s)
				reportServiceInfo(fmt.Sprintf("%s stopped", serviceDisplayName))
				h.exitCode = exitOK
				return false, 0
			}
		}
	}
}

// drain waits for the agent to finish shutting down — the unsent-telemetry
// spill happens on that path — while telling the control manager that
// progress is being made. A poll already in flight can take a few seconds to
// notice the cancellation, and a service that goes silent past its wait hint
// is treated as hung and killed, which is exactly when the spill would be lost.
func (h *serviceHandler) drain(done <-chan int, s chan<- svc.Status) {
	const tick = 2 * time.Second
	checkpoint := uint32(1)
	status := func() svc.Status {
		return svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: uint32(3 * tick / time.Millisecond)}
	}
	s <- status()
	for {
		select {
		case <-done:
			return
		case <-time.After(tick):
			checkpoint++
			s <- status()
		}
	}
}

// serviceCommand runs one of the service-management commands and returns the
// process exit code.
func serviceCommand(cmd string, opts options) int {
	var err error
	switch cmd {
	case "install":
		err = installService(opts)
	case "uninstall":
		err = uninstallService()
	case "start":
		err = startService()
	case "stop":
		err = stopService()
	case "restart":
		if err = stopService(); err == nil {
			err = startService()
		}
	case "status":
		err = printServiceStatus()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "overwatch-agent: %v\n", err)
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			fmt.Fprintln(os.Stderr, "Run this from an elevated prompt (Run as administrator).")
		}
		return exitFailure
	}
	return exitOK
}

func installService(opts options) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this executable: %w", err)
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return fmt.Errorf("locate this executable: %w", err)
	}
	dataDir := platform.DataDir() // --data-dir has already been exported by main
	if err := checkDataDir(dataDir); err != nil {
		return err
	}
	configPath := opts.configPath
	if configPath == "" {
		configPath = filepath.Join(dataDir, "agent.env")
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("open service control manager: %w", err)
	}
	defer m.Disconnect()

	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		return fmt.Errorf("service %s is already installed — run `overwatch-agent uninstall` first", serviceName)
	}

	if err := os.MkdirAll(filepath.Join(dataDir, "logs"), 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	// Ownership first: %ProgramData% lets any user create a folder, and its
	// creator keeps the right to rewrite the permissions on it however they
	// are set afterwards. Nothing sensitive is written until this holds.
	if err := icacls(dataDir, "/setowner", sidAdministrators, "/T", "/C"); err != nil {
		return fmt.Errorf("take ownership of %s: %w", dataDir, err)
	}

	// The data directory is granted to the service's own identity, so the
	// service is created (and its SID thereby defined) before the grant.
	args := []string{"run", "--data-dir", dataDir}
	if opts.configPath != "" {
		args = append(args, "--config", configPath)
	}
	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName:      serviceDisplayName,
		Description:      serviceDescription,
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		ServiceStartName: serviceAccount,
		SidType:          windows.SERVICE_SID_TYPE_UNRESTRICTED,
	}, args...)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	// Anything that fails from here leaves no half-installed service behind.
	account, wroteTemplate, err := finishInstall(s, dataDir, configPath)
	if err != nil {
		_ = s.Delete()
		return err
	}

	fmt.Printf("Installed service %s (%s).\n", serviceName, serviceDisplayName)
	fmt.Printf("  Binary:   %s\n", exe)
	fmt.Printf("  Account:  %s\n", serviceAccount)
	fmt.Printf("  Data:     %s   (%s)\n", dataDir, aclSummary(account))
	if wroteTemplate {
		fmt.Printf("  Config:   %s   (template written — fill it in before starting)\n", configPath)
	} else {
		fmt.Printf("  Config:   %s\n", configPath)
	}
	fmt.Printf("  Log:      %s\n", filepath.Join(dataDir, "logs", "agent.log"))
	for _, w := range installWarnings(exe, configPath, dataDir, wroteTemplate) {
		fmt.Printf("\nWarning: %s\n", w)
	}
	fmt.Println()
	fmt.Println("Next: edit the config as administrator, then run:  overwatch-agent start")
	return nil
}

// finishInstall does the work that follows creating the service: the data
// directory's permissions (which need the service to exist, for its SID), the
// starter configuration, the restart-on-failure actions and the event log
// source. Its caller deletes the service if any of it fails.
func finishInstall(s *mgr.Service, dataDir, configPath string) (account string, wroteTemplate bool, err error) {
	if account, err = secureDataDir(dataDir); err != nil {
		return "", false, err
	}
	if wroteTemplate, err = writeConfigTemplate(configPath, dataDir, account); err != nil {
		return "", false, err
	}
	// Restart on failure — after 5 s, then 15 s, then a minute — with the
	// failure count reset after a day. Non-crash failures (a non-zero exit)
	// count too: that is how a reboot_agent command, and a startup that failed
	// on a bad setting, both come back once the cause is fixed.
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
		{Type: mgr.ServiceRestart, Delay: time.Minute},
	}, 24*60*60); err != nil {
		return "", false, fmt.Errorf("set recovery actions: %w", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return "", false, fmt.Errorf("set recovery on exit: %w", err)
	}
	_ = eventlog.Remove(serviceName) // a source left behind by an earlier install
	if err := eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		return "", false, fmt.Errorf("register event log source: %w", err)
	}
	return account, wroteTemplate, nil
}

// checkDataDir refuses a data directory whose permissions the installer has no
// business rewriting. Installing locks the directory down to administrators
// and the service, and that is right for a folder of the agent's own and
// wrong for a drive root, a system folder, or somewhere with other people's
// files in it.
func checkDataDir(dir string) error {
	if dir == "" {
		return errors.New("no data directory — pass --data-dir")
	}
	clean := filepath.Clean(dir)
	if filepath.Dir(clean) == clean {
		return fmt.Errorf("refusing to use the drive root %s as the data directory — give the agent a folder of its own", clean)
	}
	for _, name := range []string{"ProgramData", "SystemRoot", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "PUBLIC", "USERPROFILE", "SystemDrive"} {
		if v := os.Getenv(name); v != "" && strings.EqualFold(filepath.Clean(v), clean) {
			return fmt.Errorf("refusing to use %%%s%% itself (%s) as the data directory — give the agent a folder of its own", name, clean)
		}
	}
	entries, err := os.ReadDir(clean)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", clean, err)
	}
	for _, e := range entries {
		switch strings.ToLower(e.Name()) {
		case "agent.env", "cache", "logs", "buffer.json", "packir-buffer.json":
		default:
			return fmt.Errorf("%s already contains %q, so it is not the agent's own directory; installing would restrict it to administrators and the service account. Choose an empty or dedicated folder with --data-dir", clean, e.Name())
		}
	}
	return nil
}

// secureDataDir replaces the data directory's permissions: inheritance off,
// administrators and the system in full, and the agent's own service identity
// able to write. It holds the site token and, with the cache enabled, the
// venue's game data including player details.
//
// The per-service identity is preferred and LocalService is the fallback, for
// a Windows old enough — or a machine configured oddly enough — not to resolve
// the service SID by name. Each attempt resets the whole list, so a partial
// first attempt cannot survive into the second.
func secureDataDir(dir string) (string, error) {
	var lastErr error
	for _, account := range []string{serviceSIDName, sidLocalService} {
		err := icacls(dir, "/inheritance:r", "/grant:r",
			sidAdministrators+":(OI)(CI)F",
			sidSystem+":(OI)(CI)F",
			account+":(OI)(CI)M",
		)
		if err == nil {
			return account, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("restrict permissions on %s: %w", dir, lastErr)
}

// writeConfigTemplate writes the starter configuration if none exists yet. A
// file inside the data directory inherits the permissions just set; one placed
// elsewhere is restricted on its own, because it is about to hold the site's
// token.
func writeConfigTemplate(configPath, dataDir, account string) (bool, error) {
	_, err := os.Stat(configPath)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("check %s: %w", configPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return false, fmt.Errorf("create %s: %w", filepath.Dir(configPath), err)
	}
	if err := os.WriteFile(configPath, []byte(envTemplate), 0o600); err != nil {
		return false, fmt.Errorf("write %s: %w", configPath, err)
	}
	if !underDir(dataDir, configPath) {
		// Read is all the agent needs of it.
		if err := icacls(configPath, "/inheritance:r", "/grant:r",
			sidAdministrators+":F", sidSystem+":F", account+":R"); err != nil {
			return true, fmt.Errorf("restrict permissions on %s: %w", configPath, err)
		}
	}
	return true, nil
}

// installWarnings are the things worth saying out loud once the service is in
// place: they do not stop the install, but they leave the venue less protected
// than it should be.
func installWarnings(exe, configPath, dataDir string, wroteTemplate bool) []string {
	var out []string
	if !underProgramFiles(exe) {
		out = append(out, fmt.Sprintf("the executable is outside Program Files (%s). The service runs it\n"+
			"as a limited account, so keep it somewhere only administrators can write — otherwise\n"+
			"a local user could replace it and have Windows run their code as the service.", exe))
	}
	if !underDir(dataDir, configPath) && !wroteTemplate {
		out = append(out, fmt.Sprintf("the configuration file is outside the data directory (%s)\n"+
			"and already existed, so its permissions were left alone. It holds the site token:\n"+
			"check that only administrators and the service account can read it.", configPath))
	}
	return out
}

func aclSummary(account string) string {
	if account == serviceSIDName {
		return "administrators, the system and this service only"
	}
	return "administrators, the system and the LocalService account only"
}

// icacls runs the Windows permissions tool by its absolute path in System32,
// rather than whatever the search path happens to find first.
func icacls(args ...string) error {
	exe := "icacls.exe"
	if root := os.Getenv("SystemRoot"); root != "" {
		exe = filepath.Join(root, "System32", "icacls.exe")
	}
	out, err := exec.Command(exe, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", filepath.Base(exe), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func underDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func underProgramFiles(path string) bool {
	for _, name := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432"} {
		if v := os.Getenv(name); v != "" && underDir(v, path) {
			return true
		}
	}
	return false
}

func uninstallService() error {
	m, s, err := openService()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()

	if err := stopAndWait(s); err != nil {
		return err
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	if err := eventlog.Remove(serviceName); err != nil {
		fmt.Fprintf(os.Stderr, "warning: event log source not removed: %v\n", err)
	}
	fmt.Printf("Removed service %s. The configuration, cache and log files were left in place.\n", serviceName)
	return nil
}

func startService() error {
	m, s, err := openService()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()

	if err := s.Start(); err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			fmt.Printf("%s is already running.\n", serviceDisplayName)
			return nil
		}
		return fmt.Errorf("start service: %w", err)
	}
	deadline := time.Now().Add(stopTimeout)
	for {
		st, err := s.Query()
		if err != nil {
			return fmt.Errorf("query service: %w", err)
		}
		switch st.State {
		case svc.Running:
			fmt.Printf("Started %s.\n", serviceDisplayName)
			return nil
		case svc.Stopped:
			return fmt.Errorf("the service stopped straight after starting — check the log file under %s and the Application event log", platform.DataDir())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the service did not report running within %s (state: %s)", stopTimeout, stateName(st.State))
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func stopService() error {
	m, s, err := openService()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()

	if err := stopAndWait(s); err != nil {
		return err
	}
	fmt.Printf("Stopped %s.\n", serviceDisplayName)
	return nil
}

func printServiceStatus() error {
	m, s, err := openServiceReadOnly()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()

	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("query service: %w", err)
	}
	cfg, err := s.Config()
	if err != nil {
		return fmt.Errorf("read service configuration: %w", err)
	}
	fmt.Printf("Service:  %s (%s)\n", serviceName, cfg.DisplayName)
	fmt.Printf("State:    %s\n", stateName(st.State))
	fmt.Printf("Start:    %s\n", startTypeName(cfg.StartType))
	fmt.Printf("Account:  %s\n", cfg.ServiceStartName)
	fmt.Printf("Command:  %s\n", cfg.BinaryPathName)
	fmt.Printf("Version:  %s (this executable)\n", version.Value)
	return nil
}

// openService opens the service with full access, which needs elevation.
func openService() (*mgr.Mgr, *mgr.Service, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, nil, fmt.Errorf("open service control manager: %w", err)
	}
	s, err := m.OpenService(serviceName)
	if err != nil {
		_ = m.Disconnect()
		return nil, nil, notInstalledOr(err)
	}
	return m, s, nil
}

// openServiceReadOnly opens the service to query it, which any user may do —
// so `status` works from an ordinary prompt.
func openServiceReadOnly() (*mgr.Mgr, *mgr.Service, error) {
	h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, nil, fmt.Errorf("open service control manager: %w", err)
	}
	m := &mgr.Mgr{Handle: h}
	name, err := windows.UTF16PtrFromString(serviceName)
	if err != nil {
		_ = m.Disconnect()
		return nil, nil, err
	}
	sh, err := windows.OpenService(h, name, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		_ = m.Disconnect()
		return nil, nil, notInstalledOr(err)
	}
	return m, &mgr.Service{Name: serviceName, Handle: sh}, nil
}

func notInstalledOr(err error) error {
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return fmt.Errorf("service %s is not installed — run `overwatch-agent install` from an elevated prompt", serviceName)
	}
	return fmt.Errorf("open service: %w", err)
}

// stopAndWait stops the service and waits for it to reach Stopped. It expects
// to be racing whoever else may be stopping or starting it: a service that
// stops on its own between the query and the request is stopped, not an
// error, and one that is still starting is asked again once it can accept the
// request.
func stopAndWait(s *mgr.Service) error {
	deadline := time.Now().Add(stopTimeout)
	requested := false
	for {
		st, err := s.Query()
		if err != nil {
			return fmt.Errorf("query service: %w", err)
		}
		if st.State == svc.Stopped {
			return nil
		}
		if !requested && st.State != svc.StopPending {
			switch _, err := s.Control(svc.Stop); {
			case err == nil:
				requested = true
			case errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE):
				return nil
			case errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL):
				// Mid-start or mid-stop: ask again on the next pass.
			default:
				return fmt.Errorf("stop service: %w", err)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the service did not stop within %s (state: %s)", stopTimeout, stateName(st.State))
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func stateName(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "resuming"
	case svc.PausePending:
		return "pausing"
	case svc.Paused:
		return "paused"
	}
	return fmt.Sprintf("state %d", s)
}

func startTypeName(t uint32) string {
	switch t {
	case mgr.StartAutomatic:
		return "automatic"
	case mgr.StartManual:
		return "manual"
	case mgr.StartDisabled:
		return "disabled"
	}
	return fmt.Sprintf("type %d", t)
}

// The event log is where a service reports what it cannot print: it starts
// and stops, and why it would not start. Everything else goes to the log file.

func reportServiceInfo(msg string) {
	reportEvent(func(l *eventlog.Log) error { return l.Info(1, msg) })
}

func reportServiceError(msg string) {
	reportEvent(func(l *eventlog.Log) error { return l.Error(1, msg) })
}

func reportEvent(fn func(*eventlog.Log) error) {
	l, err := eventlog.Open(serviceName)
	if err != nil {
		return
	}
	defer l.Close()
	_ = fn(l)
}
