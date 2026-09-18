package releasemanager

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

func NewLayout(component string) (*Layout, error) {
	// Gateway services use system paths and may run without HOME under systemd.
	// Only the worker installation depends on the invoking user's home.
	var home string
	if component == "worker" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}
	return newLayout(component, runtime.GOOS, runtime.GOARCH, home)
}

func newLayout(component, system, architecture, home string) (*Layout, error) {
	if (component != "gateway" && component != "worker") || (system != "linux" && system != "darwin") || (architecture != "amd64" && architecture != "arm64") || (component == "gateway" && system != "linux") {
		return nil, errors.New("unsupported component or platform")
	}
	if component == "worker" && (!filepath.IsAbs(home) || strings.ContainsAny(home, "%\"\\") || strings.ContainsFunc(home, unicode.IsSpace)) {
		return nil, errors.New("service installation requires an absolute home path without spaces, quotes, percent signs or backslashes")
	}
	l := &Layout{Component: component, System: system, Architecture: architecture, Home: home}
	if component == "gateway" {
		l.Bin = "/usr/local/lib/codex-telegramgw"
		l.Manager = filepath.Join(l.Bin, "codex-telegramgw")
		l.LegacyManager = filepath.Join(l.Bin, "release-manager.py")
		l.Command = "/usr/local/bin/codex-telegramgw"
		l.Config = "/etc/codex-gateway/gateway.json"
		l.Environment = "/etc/codex-gateway/secrets.env"
		l.State = "/etc/codex-gateway/update.json"
		l.Lock = "/run/lock/codex-telegramgw-update.lock"
		l.Backups = "/var/backups/codex-gateway/updates"
		l.UnitDir = "/etc/systemd/system"
		l.DataRoot = GatewayDataRoot
	} else {
		l.Bin = filepath.Join(home, ".local/bin")
		l.Manager = filepath.Join(home, ".local/lib/codex-telegramgw/codex-telegramgw")
		l.LegacyManager = filepath.Join(home, ".local/lib/codex-telegramgw/release-manager.py")
		l.Command = filepath.Join(l.Bin, "codex-telegramgw")
		l.Config = filepath.Join(home, ".config/codex-worker/config.json")
		l.State = filepath.Join(home, ".config/codex-worker/update.json")
		l.Lock = filepath.Join(home, ".local/state/codex-worker/update.lock")
		l.Backups = filepath.Join(home, ".local/state/codex-worker/updates")
		l.UnitDir = filepath.Join(home, ".config/systemd/user")
	}
	l.Binary = filepath.Join(l.Bin, "codex-"+component)
	l.Unit = filepath.Join(l.UnitDir, "codex-"+component+".service")
	return l, nil
}

func (l *Layout) RequireUser() error {
	if l.Component == "gateway" {
		if os.Geteuid() != 0 {
			return errors.New("gateway installation and updates require sudo")
		}
		for _, path := range []string{l.State, l.Config, filepath.Dir(l.Config), filepath.Dir(l.Manager)} {
			info, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || info.Mode()&os.ModeSymlink != 0 || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
				return errors.New("gateway installer paths must be root-owned and not group/world writable")
			}
		}
	} else if os.Geteuid() == 0 {
		return errors.New("run worker installation and updates as the worker account, without sudo")
	}
	return nil
}

func systemctlPrefix(l *Layout) []string {
	if l.Component == "worker" {
		return []string{"systemctl", "--user"}
	}
	return []string{"systemctl"}
}

func workerPlist(l *Layout) string {
	return filepath.Join(l.Home, "Library/LaunchAgents/com.iaia.codex-worker.plist")
}

func (m *Manager) Service(ctx context.Context, l *Layout, action string) error {
	if l.System == "linux" {
		_, err := m.command(ctx, append(systemctlPrefix(l), action, "codex-"+l.Component+".service")...)
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	label := domain + "/com.iaia.codex-worker"
	if action == "stop" {
		_, err := m.command(ctx, "launchctl", "bootout", label)
		return err
	}
	if action == "restart" {
		_, _ = m.Run(ctx, "launchctl", "bootout", label)
	}
	if action == "start" || action == "restart" || action == "enable" {
		_, err := m.command(ctx, "launchctl", "bootstrap", domain, workerPlist(l))
		return err
	}
	return errors.New("unsupported launchd operation")
}

// InstallManager accepts only our existing command links, including the legacy
// Python manager link. Replacing the link atomically keeps updates executable.
func (m *Manager) InstallManager(l *Layout, source string) error {
	info, err := os.Lstat(l.Command)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && l.Command != l.Manager {
		if info.Mode()&os.ModeSymlink == 0 {
			return errors.New("manager command path is already occupied")
		}
		target, err := os.Readlink(l.Command)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(l.Command), target)
		}
		target = filepath.Clean(target)
		if target != filepath.Clean(l.Manager) && (l.LegacyManager == "" || target != filepath.Clean(l.LegacyManager)) {
			return errors.New("manager command path is already occupied")
		}
	}
	if err := AtomicCopy(source, l.Manager, 0755); err != nil {
		return err
	}
	if l.Command == l.Manager {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(l.Command), 0755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(l.Command), ".codex-telegramgw-link-")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Remove(name); err != nil {
		return err
	}
	if err := os.Symlink(l.Manager, name); err != nil {
		return err
	}
	if err := os.Rename(name, l.Command); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(l.Command))
}

func gatewayOwnership() (*Ownership, error) {
	account, err := user.Lookup("codexgateway")
	if err != nil {
		return nil, err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return nil, err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return nil, err
	}
	return &Ownership{UID: uid, GID: gid}, nil
}

// canonicalPath resolves all existing parent symlinks, including for a database
// that has not been created yet. This prevents escaping the service data root.
func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, statErr := os.Lstat(absolute); statErr == nil {
		return "", fmt.Errorf("cannot resolve path %s", absolute)
	}
	parent := filepath.Dir(absolute)
	if parent == absolute {
		return "", err
	}
	resolved, err = canonicalPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(absolute)), nil
}

func withinDirectory(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func gatewayDatabase(l *Layout, cfg map[string]any, source string) (string, error) {
	path, _ := cfg["database_path"].(string)
	_, postgres := cfg["database_url_env"]
	if postgres || path == "" {
		return "", errors.New("provide a SQLite gateway configuration; PostgreSQL migration is a separate operation")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(source), path)
	}
	resolved, err := canonicalPath(path)
	if err != nil {
		return "", err
	}
	root := l.DataRoot
	if root == "" {
		root = GatewayDataRoot
	}
	if !withinDirectory(root, resolved) || filepath.Clean(root) == resolved {
		return "", errors.New("gateway database_path must be inside /var/lib/codex-gateway for the hardened service")
	}
	return resolved, nil
}

func regularNoSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func (m *Manager) SecureGatewayAdoption(l *Layout) error {
	if os.Geteuid() != 0 {
		return errors.New("gateway adoption requires sudo")
	}
	dir, err := os.Lstat(filepath.Dir(l.Config))
	if err != nil || !dir.IsDir() || !regularNoSymlink(l.Config) || !regularNoSymlink(l.Environment) {
		return errors.New("gateway adoption requires regular configuration files in the standard directory")
	}
	cfg, err := ReadJSON(l.Config)
	if err != nil {
		return err
	}
	if _, err := gatewayDatabase(l, cfg, l.Config); err != nil {
		return err
	}
	path, _ := cfg["database_path"].(string)
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(l.Config), path)
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("gateway adoption requires a regular SQLite database path")
	}
	owner, err := gatewayOwnership()
	if err != nil {
		return err
	}
	for _, entry := range []struct {
		path string
		mode os.FileMode
		gid  int
	}{
		{filepath.Dir(l.Config), 0750, owner.GID}, {l.Config, 0640, owner.GID}, {l.Environment, 0600, 0},
	} {
		if err := os.Chown(entry.path, 0, entry.gid); err != nil {
			return err
		}
		if err := os.Chmod(entry.path, entry.mode); err != nil {
			return err
		}
	}
	return nil
}

func normalizeGateway(l *Layout, source, environment string) error {
	cfg, err := ReadJSON(source)
	if err != nil {
		return err
	}
	database, err := gatewayDatabase(l, cfg, source)
	if err != nil {
		return err
	}
	bot, _ := cfg["bot_secrets_file"].(string)
	if bot == "" {
		bot = ".botsecrets"
	}
	if !filepath.IsAbs(bot) {
		bot = filepath.Join(filepath.Dir(source), bot)
	}
	info, err := os.Stat(bot)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("bot secrets file must be a regular file with mode 0600")
	}
	if environment == "" || !regularNoSymlink(environment) {
		return errors.New("gateway install requires --secrets-env PATH")
	}
	if name, _ := cfg["webhook_secret_env"].(string); name == "" {
		return errors.New("gateway configuration must name webhook_secret_env")
	}
	botData, err := os.ReadFile(bot)
	if err != nil {
		return err
	}
	envData, err := os.ReadFile(environment)
	if err != nil {
		return err
	}
	owner, err := gatewayOwnership()
	if err != nil {
		return err
	}
	directory := filepath.Dir(l.Config)
	if err := os.MkdirAll(directory, 0750); err != nil {
		return err
	}
	if err := os.Chown(directory, 0, owner.GID); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0750); err != nil {
		return err
	}
	root := l.DataRoot
	if root == "" {
		root = GatewayDataRoot
	}
	if err := os.MkdirAll(filepath.Dir(database), 0700); err != nil {
		return err
	}
	for path := filepath.Dir(database); withinDirectory(root, path); path = filepath.Dir(path) {
		if err := os.Chown(path, owner.UID, owner.GID); err != nil {
			return err
		}
		if err := os.Chmod(path, 0700); err != nil {
			return err
		}
		if path == filepath.Clean(root) {
			break
		}
	}
	cfg["database_path"] = database
	cfg["bot_secrets_file"] = filepath.Join(directory, ".botsecrets")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := AtomicWrite(l.Config, append(data, '\n'), 0640, &Ownership{UID: 0, GID: owner.GID}); err != nil {
		return err
	}
	if err := writePrivateSecret(filepath.Join(directory, ".botsecrets"), botData, owner); err != nil {
		return err
	}
	return writePrivateSecret(l.Environment, envData, &Ownership{UID: 0, GID: 0})
}

// AtomicWrite normally preserves existing permissions. An orphaned secret file
// must first be made private so retries cannot retain permissive old modes.
func writePrivateSecret(path string, data []byte, owner *Ownership) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("refusing to replace a non-regular secret file")
		}
		if err := os.Chmod(path, 0600); err != nil {
			return err
		}
		if owner != nil && os.Geteuid() == 0 {
			if err := os.Chown(path, owner.UID, owner.GID); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return AtomicWrite(path, data, 0600, owner)
}

func workerExecutablePaths(cfg map[string]any, home string) (string, error) {
	paths := []string{}
	profiles, _ := cfg["runtimes"].([]any)
	for _, value := range profiles {
		profile, ok := value.(map[string]any)
		if !ok {
			return "", errors.New("invalid worker runtime configuration")
		}
		executable, _ := profile["codex_binary"].(string)
		if executable == "" {
			return "", errors.New("configured Codex executable is missing")
		}
		if !strings.ContainsRune(executable, '/') {
			resolved, err := exec.LookPath(executable)
			if err != nil {
				return "", errors.New("Codex executable is unavailable in the installing account PATH")
			}
			executable = resolved
		}
		executable, err := filepath.Abs(executable)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(executable)
		if err != nil || !info.Mode().IsRegular() || syscall.Access(executable, 1) != nil {
			return "", errors.New("configured Codex executable is not executable")
		}
		profile["codex_binary"] = executable
		paths = append(paths, filepath.Dir(executable))
	}
	if node, err := exec.LookPath("node"); err == nil {
		paths = append(paths, filepath.Dir(node))
	}
	paths = append(paths, filepath.Join(home, ".local/bin"), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin")
	unique := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, path := range paths {
		if strings.ContainsAny(path, "\n\r%\"\\:") {
			return "", errors.New("executable directories contain unsupported service PATH characters")
		}
		if !seen[path] {
			unique = append(unique, path)
			seen[path] = true
		}
	}
	return strings.Join(unique, ":"), nil
}

func (m *Manager) installConfiguration(ctx context.Context, l *Layout, source, environment, pkg string) error {
	if _, err := os.Lstat(l.Config); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return errors.New("existing configuration found; use adopt to manage this installation without replacing configuration")
	}
	if l.Component == "gateway" {
		if _, err := user.Lookup("codexgateway"); err != nil {
			var unknown user.UnknownUserError
			if !errors.As(err, &unknown) {
				return err
			}
			if _, err := m.command(ctx, "useradd", "--system", "--user-group", "--home-dir", GatewayDataRoot, "--shell", "/usr/sbin/nologin", "codexgateway"); err != nil {
				return err
			}
		}
		return normalizeGateway(l, source, environment)
	}
	data, err := m.command(ctx, filepath.Join(pkg, "codex-worker"), "--config", source, "config", "export")
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil || cfg == nil {
		return errors.New("worker config export did not return a JSON object")
	}
	if _, err := workerExecutablePaths(cfg, l.Home); err != nil {
		return err
	}
	token, _ := cfg["token_file"].(string)
	state, _ := cfg["state_file"].(string)
	if !filepath.IsAbs(token) || !filepath.IsAbs(state) {
		return errors.New("worker config export must contain absolute token_file and state_file paths")
	}
	tokenData, err := os.ReadFile(token)
	if err != nil {
		return err
	}
	privateToken := filepath.Join(filepath.Dir(l.Config), "worker.token")
	if err := writePrivateSecret(privateToken, tokenData, nil); err != nil {
		return err
	}
	cfg["token_file"] = privateToken
	if err := WriteJSON(l.Config, cfg); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Dir(state), 0700)
}

func (m *Manager) installService(ctx context.Context, l *Layout, pkg string) error {
	if l.System == "linux" {
		data, err := os.ReadFile(filepath.Join(pkg, "deploy/systemd", filepath.Base(l.Unit)))
		if err != nil {
			return err
		}
		if l.Component == "worker" {
			cfg, err := ReadJSON(l.Config)
			if err != nil {
				return err
			}
			path, err := workerExecutablePaths(cfg, l.Home)
			if err != nil {
				return err
			}
			data = regexp.MustCompile(`(?m)^Environment=PATH=.*$`).ReplaceAllLiteral(data, []byte(`Environment="PATH=`+path+`"`))
		}
		if err := AtomicWrite(l.Unit, data, 0644, nil); err != nil {
			return err
		}
		_, err = m.command(ctx, append(systemctlPrefix(l), "daemon-reload")...)
		return err
	}
	cfg, err := ReadJSON(l.Config)
	if err != nil {
		return err
	}
	path, err := workerExecutablePaths(cfg, l.Home)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(l.Home, "Library/Logs"), 0755); err != nil {
		return err
	}
	value := map[string]any{
		"Label": "com.iaia.codex-worker", "ProgramArguments": []string{l.Binary, "--config", l.Config, "run"},
		"EnvironmentVariables": map[string]any{"HOME": l.Home, "PATH": path},
		"WorkingDirectory":     l.Home, "RunAtLoad": true, "KeepAlive": true,
		"StandardOutPath":   filepath.Join(l.Home, "Library/Logs/codex-worker.log"),
		"StandardErrorPath": filepath.Join(l.Home, "Library/Logs/codex-worker.log"),
	}
	return writePlist(workerPlist(l), value)
}

func (m *Manager) FreshInstall(ctx context.Context, l *Layout, source, environment string, packages map[string]string, release *Release) (result error) {
	servicePath := l.Unit
	if l.System == "darwin" {
		servicePath = workerPlist(l)
	}
	for _, path := range []string{l.Config, servicePath} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return err
			}
			return errors.New("existing configuration/service found; use adopt to manage this installation")
		}
	}
	secret := ".botsecrets"
	if l.Component == "worker" {
		secret = "worker.token"
	}
	paths := []string{l.Config, servicePath, l.State, l.Command, l.Manager, filepath.Join(filepath.Dir(l.Config), secret)}
	if l.Environment != "" {
		paths = append(paths, l.Environment)
	}
	existed := make(map[string]bool, len(paths))
	for _, path := range paths {
		_, err := os.Lstat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		existed[path] = err == nil
	}
	defer func() {
		if result == nil {
			return
		}
		var applied *ApplyError
		if errors.As(result, &applied) && applied.ServiceStarted {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if FileExists(servicePath) && !existed[servicePath] {
			if l.System == "linux" {
				_, _ = m.Run(cleanupCtx, append(systemctlPrefix(l), "disable", "--now", filepath.Base(servicePath))...)
			} else {
				_ = m.Service(cleanupCtx, l, "stop")
			}
		}
		for _, path := range paths {
			if !existed[path] {
				_ = os.Remove(path)
			}
		}
		if l.System == "linux" {
			_, _ = m.Run(cleanupCtx, append(systemctlPrefix(l), "daemon-reload")...)
		}
	}()
	if err := m.installConfiguration(ctx, l, source, environment, packages[l.Component]); err != nil {
		return err
	}
	if err := m.installService(ctx, l, packages[l.Component]); err != nil {
		return err
	}
	if l.System == "linux" {
		if err := m.Service(ctx, l, "enable"); err != nil {
			return err
		}
	}
	return m.ApplyUpdate(ctx, l, packages, release, true)
}

func (m *Manager) AutoUpdate(ctx context.Context, l *Layout, enabled bool) error {
	label := "codex-" + l.Component + "-update"
	if l.System == "linux" {
		prefix := systemctlPrefix(l)
		if enabled {
			service := "[Unit]\nDescription=Update Codex Telegram " + l.Component + " from GitHub Releases\nWants=network-online.target\nAfter=network-online.target\n\n[Service]\nType=oneshot\nExecStart=" + l.Command + " update " + l.Component + "\nSuccessExitStatus=75\nUMask=0077\nTimeoutStartSec=15min\n"
			calendar := "*-*-* *:00/5:00"
			if l.Component == "worker" {
				calendar = "*-*-* *:02/5:00"
			}
			timer := "[Unit]\nDescription=Check every five minutes for Codex Telegram " + l.Component + " releases\n\n[Timer]\nOnCalendar=" + calendar + "\nRandomizedDelaySec=0\nAccuracySec=1s\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n"
			if err := AtomicWrite(filepath.Join(l.UnitDir, label+".service"), []byte(service), 0644, nil); err != nil {
				return err
			}
			if err := AtomicWrite(filepath.Join(l.UnitDir, label+".timer"), []byte(timer), 0644, nil); err != nil {
				return err
			}
			if _, err := m.command(ctx, append(prefix, "daemon-reload")...); err != nil {
				return err
			}
			if _, err := m.command(ctx, append(prefix, "enable", "--now", label+".timer")...); err != nil {
				return err
			}
		} else if _, err := m.command(ctx, append(prefix, "disable", "--now", label+".timer")...); err != nil {
			return err
		}
	} else {
		label = "com.iaia.codex-worker-update"
		plist := filepath.Join(l.Home, "Library/LaunchAgents", label+".plist")
		domain := "gui/" + strconv.Itoa(os.Getuid())
		_, _ = m.Run(ctx, "launchctl", "bootout", domain+"/"+label)
		if enabled {
			calendar := make([]any, 0, 12)
			for minute := 2; minute < 60; minute += 5 {
				calendar = append(calendar, map[string]any{"Minute": minute})
			}
			value := map[string]any{
				"Label": label, "ProgramArguments": []string{l.Command, "update", "worker"},
				"StartCalendarInterval": calendar,
				"EnvironmentVariables":  map[string]any{"HOME": l.Home, "PATH": l.Bin + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"},
			}
			if err := writePlist(plist, value); err != nil {
				return err
			}
			if _, err := m.command(ctx, "launchctl", "bootstrap", domain, plist); err != nil {
				return err
			}
		} else if err := os.Remove(plist); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if m.Out != nil {
		state := "disabled"
		if enabled {
			state = "enabled"
		}
		fmt.Fprintf(m.Out, "Automatic update checks every five minutes %s for %s.\n", state, l.Component)
		if enabled {
			fmt.Fprintln(m.Out, "Gateway checks at minutes 00, 05, 10, ...; workers check two minutes later at 02, 07, 12, ... (local time).")
		}
	}
	return nil
}

func writePlist(path string, value map[string]any) error {
	var out bytes.Buffer
	out.WriteString(xml.Header)
	out.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\">")
	if err := encodePlistValue(&out, value); err != nil {
		return err
	}
	out.WriteString("</plist>\n")
	return AtomicWrite(path, out.Bytes(), 0644, nil)
}

func encodePlistValue(out *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case string:
		out.WriteString("<string>")
		if err := xml.EscapeText(out, []byte(v)); err != nil {
			return err
		}
		out.WriteString("</string>")
	case bool:
		if v {
			out.WriteString("<true/>")
		} else {
			out.WriteString("<false/>")
		}
	case int:
		fmt.Fprintf(out, "<integer>%d</integer>", v)
	case []string:
		out.WriteString("<array>")
		for _, item := range v {
			if err := encodePlistValue(out, item); err != nil {
				return err
			}
		}
		out.WriteString("</array>")
	case []any:
		out.WriteString("<array>")
		for _, item := range v {
			if err := encodePlistValue(out, item); err != nil {
				return err
			}
		}
		out.WriteString("</array>")
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteString("<dict>")
		for _, key := range keys {
			out.WriteString("<key>")
			if err := xml.EscapeText(out, []byte(key)); err != nil {
				return err
			}
			out.WriteString("</key>")
			if err := encodePlistValue(out, v[key]); err != nil {
				return err
			}
		}
		out.WriteString("</dict>")
	default:
		return errors.New("unsupported property-list value")
	}
	return nil
}
