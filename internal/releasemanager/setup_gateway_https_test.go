package releasemanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/config"
)

func gatewayHTTPSFixture(t *testing.T) (*Manager, config.GatewayConfig, gatewayHTTPSPaths, *[][]string) {
	t.Helper()
	root := t.TempDir()
	paths := gatewayHTTPSPaths{
		OSRelease: filepath.Join(root, "os-release"), Config: filepath.Join(root, "etc/codex-gateway-proxy/Caddyfile"),
		Unit: filepath.Join(root, "systemd/codex-gateway-proxy.service"), State: filepath.Join(root, "etc/codex-gateway-proxy/setup.json"),
		ExistingProxyPaths: []string{filepath.Join(root, "etc/caddy"), filepath.Join(root, "etc/nginx")},
		PortsAvailable:     func() bool { return true }, PackageManagerAvailable: func() bool { return true },
	}
	platformWrite(t, paths.OSRelease, "ID=ubuntu\n", 0644)
	m := New(nil)
	var commands [][]string
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		commands = append(commands, append([]string(nil), args...))
		if len(args) > 1 && args[1] == "is-active" {
			return CommandResult{ExitCode: 3}, nil
		}
		return CommandResult{}, nil
	}
	m.SetupRun = func(_ context.Context, args ...string) (CommandResult, error) {
		commands = append(commands, append([]string(nil), args...))
		return CommandResult{}, nil
	}
	return m, config.GatewayConfig{PublicBaseURL: "https://gateway.example.com", Listen: "127.0.0.1:8080"}, paths, &commands
}

func TestGatewayAutomaticHTTPSInstallsDedicatedProxyAndResumes(t *testing.T) {
	m, cfg, paths, commands := gatewayHTTPSFixture(t)
	if err := m.setupGatewayHTTPS(context.Background(), cfg, paths); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(paths.Config)
	if err != nil || !strings.Contains(string(data), "handle @gateway") || !strings.Contains(string(data), "reverse_proxy \"127.0.0.1:8080\"") || !strings.Contains(string(data), "admin off") {
		t.Fatal("incorrect HTTPS proxy configuration", err)
	}
	if strings.Contains(string(data), "rewrite") || strings.Contains(string(data), "handle_path") {
		t.Fatal("gateway paths would be stripped by the proxy")
	}
	unit, _ := os.ReadFile(paths.Unit)
	for _, want := range []string{"User=caddy", "Group=caddy", "HOME=/var/lib/caddy", "CAP_NET_BIND_SERVICE", "ProtectHome=true", "--adapter caddyfile"} {
		if !strings.Contains(string(unit), want) {
			t.Fatalf("missing service setting %s", want)
		}
	}
	if !privateRegularSetupFile(paths.State) {
		t.Fatal("HTTPS installation state is not private")
	}
	var apt [][]string
	for _, command := range *commands {
		if command[0] == "apt-get" {
			apt = append(apt, command)
		}
	}
	if !reflect.DeepEqual(apt, [][]string{{"apt-get", "update"}, {"apt-get", "install", "--yes", "caddy"}}) {
		t.Fatalf("package installation = %v", apt)
	}
	*commands = nil
	// An installed proxy now occupies the ports and has a package config. A
	// repeat invocation uses only its dedicated files/service.
	paths.PortsAvailable = func() bool { return false }
	platformWrite(t, filepath.Join(paths.ExistingProxyPaths[0], "Caddyfile"), "package original", 0644)
	if err := m.setupGatewayHTTPS(context.Background(), cfg, paths); err != nil {
		t.Fatal(err)
	}
	for _, command := range *commands {
		if command[0] == "apt-get" || (command[0] == "systemctl" && command[1] == "disable") {
			t.Fatalf("repeat setup changed package/default Caddy service: %v", command)
		}
	}
}

func TestGatewayAutomaticHTTPSRefusesExistingServersAndUnsupportedHosts(t *testing.T) {
	for _, name := range []string{"nginx", "active-service", "ports", "custom-unit", "existing-directory", "unsupported-os", "no-apt", "ip-origin", "custom-port"} {
		t.Run(name, func(t *testing.T) {
			m, cfg, paths, commands := gatewayHTTPSFixture(t)
			switch name {
			case "nginx":
				platformWrite(t, filepath.Join(paths.ExistingProxyPaths[1], "nginx.conf"), "preserve me", 0644)
			case "active-service":
				m.Run = func(context.Context, ...string) (CommandResult, error) { return CommandResult{}, nil }
			case "ports":
				paths.PortsAvailable = func() bool { return false }
			case "custom-unit":
				platformWrite(t, paths.Unit, "preserve me", 0644)
			case "existing-directory":
				if err := os.MkdirAll(filepath.Dir(paths.Config), 0755); err != nil {
					t.Fatal(err)
				}
			case "unsupported-os":
				platformWrite(t, paths.OSRelease, "ID=fedora\n", 0644)
			case "no-apt":
				paths.PackageManagerAvailable = func() bool { return false }
			case "ip-origin":
				cfg.PublicBaseURL = "https://192.0.2.1"
			case "custom-port":
				cfg.PublicBaseURL = "https://gateway.example.com:8443"
			}
			if err := m.setupGatewayHTTPS(context.Background(), cfg, paths); err == nil {
				t.Fatal("unsafe/unsupported automatic HTTPS was accepted")
			}
			if FileExists(paths.Config) || FileExists(paths.State) {
				t.Fatal("rejected setup wrote its managed files")
			}
			for _, command := range *commands {
				if command[0] != "systemctl" || command[1] != "is-active" {
					t.Fatalf("rejected setup changed the machine: %v", command)
				}
			}
		})
	}
}

func TestGatewayAutomaticHTTPSResumesInterruptedPackageInstallation(t *testing.T) {
	m, cfg, paths, commands := gatewayHTTPSFixture(t)
	first := true
	m.SetupRun = func(_ context.Context, args ...string) (CommandResult, error) {
		*commands = append(*commands, args)
		if first && args[1] == "install" {
			first = false
			platformWrite(t, filepath.Join(paths.ExistingProxyPaths[0], "Caddyfile"), "preserve package config", 0644)
			return CommandResult{}, errors.New("interrupted package installation")
		}
		return CommandResult{}, nil
	}
	if err := m.setupGatewayHTTPS(context.Background(), cfg, paths); err == nil {
		t.Fatal("package failure was ignored")
	}
	if !FileExists(paths.State) || FileExists(paths.Config) {
		t.Fatal("partial installation did not retain resumable intent")
	}
	if err := m.setupGatewayHTTPS(context.Background(), cfg, paths); err != nil {
		t.Fatal("could not resume our own package installation", err)
	}
	data, _ := os.ReadFile(filepath.Join(paths.ExistingProxyPaths[0], "Caddyfile"))
	if string(data) != "preserve package config" {
		t.Fatal("package configuration was overwritten")
	}
}

func TestGatewayAutomaticHTTPSResumesAfterUncertainServiceStart(t *testing.T) {
	m, cfg, paths, commands := gatewayHTTPSFixture(t)
	run := m.Run
	first := true
	m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
		if first && len(args) > 1 && args[1] == "enable" {
			first = false
			return CommandResult{}, errors.New("interrupted after requesting service startup")
		}
		return run(ctx, args...)
	}
	if err := m.setupGatewayHTTPS(context.Background(), cfg, paths); err == nil {
		t.Fatal("interrupted service start was ignored")
	}
	state, err := ReadJSON(paths.State)
	if err != nil || state["phase"] != "configured" {
		t.Fatal("service start boundary was not durably recorded", err)
	}
	paths.PortsAvailable = func() bool { return false }
	*commands = nil
	if err := m.setupGatewayHTTPS(context.Background(), cfg, paths); err != nil {
		t.Fatal("our own already-bound proxy prevented recovery", err)
	}
	for _, command := range *commands {
		if command[0] == "apt-get" || (command[0] == "systemctl" && command[1] == "disable") {
			t.Fatal("configured proxy recovery repeated package installation")
		}
	}
}

func TestGatewayAutomaticHTTPSNeverOverwritesModifiedManagedFiles(t *testing.T) {
	for _, name := range []string{"config", "unit", "origin", "linked-state"} {
		t.Run(name, func(t *testing.T) {
			m, cfg, paths, commands := gatewayHTTPSFixture(t)
			if err := m.setupGatewayHTTPS(context.Background(), cfg, paths); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "config":
				platformWrite(t, paths.Config, "preserve custom proxy", 0644)
			case "unit":
				platformWrite(t, paths.Unit, "preserve custom unit", 0644)
			case "origin":
				cfg.PublicBaseURL = "https://different.example.com"
			case "linked-state":
				if err := os.Rename(paths.State, paths.State+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(paths.State+".original", paths.State); err != nil {
					t.Fatal(err)
				}
			}
			*commands = nil
			if err := m.setupGatewayHTTPS(context.Background(), cfg, paths); err == nil {
				t.Fatal("modified managed installation was accepted")
			}
			if len(*commands) != 0 {
				t.Fatal("changed services before validating managed files")
			}
		})
	}
}

func TestGatewayAutomaticHTTPSOriginValidation(t *testing.T) {
	for _, origin := range []string{"https://gateway.example.com", "https://gateway.example.com:443", "https://gw-1.example.com"} {
		if !gatewayAutomaticHTTPSOrigin(origin) {
			t.Fatalf("rejected valid origin %s", origin)
		}
	}
	for _, origin := range []string{"http://gateway.example.com", "https://localhost", "https://192.0.2.1", "https://[::1]", "https://gateway.example.com:8443", "https://bad_name.example.com", "https://gateway.example.com/path", "https://user@gateway.example.com", "https://gateway.example.com?x=y"} {
		if gatewayAutomaticHTTPSOrigin(origin) {
			t.Fatalf("accepted invalid origin %s", origin)
		}
	}
}
