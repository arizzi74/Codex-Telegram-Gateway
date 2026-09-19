package releasemanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/iaia/telegramgw/internal/config"
)

const gatewayProxyService = "codex-gateway-proxy.service"

type gatewayHTTPSPaths struct {
	OSRelease, Config, Unit, State string
	ExistingProxyPaths             []string
	PortsAvailable                 func() bool
	PackageManagerAvailable        func() bool
}

func defaultGatewayHTTPSPaths() gatewayHTTPSPaths {
	return gatewayHTTPSPaths{
		OSRelease: "/etc/os-release", Config: "/etc/codex-gateway-proxy/Caddyfile",
		Unit: "/etc/systemd/system/" + gatewayProxyService, State: "/etc/codex-gateway-proxy/setup.json",
		ExistingProxyPaths: []string{"/etc/caddy", "/etc/nginx", "/etc/apache2", "/etc/httpd", "/usr/bin/caddy", "/usr/sbin/nginx", "/usr/sbin/apache2", "/etc/systemd/system/caddy.service", "/etc/systemd/system/caddy-api.service"},
		PortsAvailable:     gatewayHTTPSPortsAvailable,
		PackageManagerAvailable: func() bool {
			_, err := exec.LookPath("apt-get")
			return err == nil
		},
	}
}

type gatewayHTTPSState struct {
	Origin string `json:"origin"`
	Listen string `json:"listen"`
	Phase  string `json:"phase"`
}

// setupGatewayHTTPS changes only files/services owned by this wizard. Existing
// proxy installations require the manual path, even if currently stopped.
func (m *Manager) setupGatewayHTTPS(ctx context.Context, cfg config.GatewayConfig, paths gatewayHTTPSPaths) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !gatewayAutomaticHTTPSOrigin(cfg.PublicBaseURL) {
		return errors.New("automatic HTTPS requires a public DNS hostname on the standard HTTPS port 443")
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil || port == "" || (host != "127.0.0.1" && host != "::1" && host != "localhost") {
		return errors.New("automatic HTTPS requires the gateway to listen on a loopback address")
	}
	caddyfile := []byte("# Managed by codex-telegramgw setup.\n{\n    admin off\n}\n\n" + gatewayCaddySite(cfg))
	unit := []byte(gatewayCaddyUnit(paths.Config))
	managed, installed := false, false
	if data, err := os.ReadFile(paths.State); err == nil {
		var state gatewayHTTPSState
		if !privateRegularSetupFile(paths.State) || json.Unmarshal(data, &state) != nil || state.Origin != cfg.PublicBaseURL || state.Listen != cfg.Listen || (state.Phase != "installing" && state.Phase != "configured" && state.Phase != "ready") {
			return errors.New("managed HTTPS settings have changed; review the proxy configuration manually")
		}
		for path, expected := range map[string][]byte{paths.Config: caddyfile, paths.Unit: unit} {
			actual, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) && state.Phase == "installing" && !FileExists(path) {
				continue
			}
			if err != nil || !regularNoSymlink(path) || !bytes.Equal(actual, expected) {
				return errors.New("managed HTTPS files have changed; refusing to replace them")
			}
		}
		managed = true
		installed = state.Phase != "installing"
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("could not read managed HTTPS setup state")
	}
	if !managed {
		data, err := os.ReadFile(paths.OSRelease)
		if err != nil || !gatewayHTTPSDebian(data) || !paths.PackageManagerAvailable() {
			return errors.New("automatic HTTPS installation is supported on Debian and Ubuntu; use an existing HTTPS reverse proxy on this system")
		}
		for _, path := range append(append([]string{}, paths.ExistingProxyPaths...), paths.Config, filepath.Dir(paths.Config), paths.Unit) {
			if FileExists(path) {
				return errors.New("an existing proxy configuration was found; choose check or manual to preserve it")
			}
		}
		// Catch installations with nonstandard configuration locations too.
		for _, service := range []string{"caddy.service", "caddy-api.service", "nginx.service", "apache2.service", "httpd.service"} {
			result, err := m.Run(ctx, "systemctl", "is-active", "--quiet", service)
			if err != nil || result.ExitCode == 0 {
				return errors.New("an existing web server may be running; choose check or manual to preserve it")
			}
		}
		if !paths.PortsAvailable() {
			return errors.New("ports 80 or 443 are already in use; choose check or manual for the existing web server")
		}
		if err := os.MkdirAll(filepath.Dir(paths.Config), 0755); err != nil {
			return err
		}
		// The service runs as caddy, so a restrictive invoking umask must not
		// make the newly created public configuration directory inaccessible.
		if err := os.Chmod(filepath.Dir(paths.Config), 0755); err != nil {
			return err
		}
		// Save ownership of this installation attempt before apt can create
		// files or start its default service. A retry can then recognize its
		// own partial installation without taking over an unrelated proxy.
		if err := WriteJSON(paths.State, gatewayHTTPSState{Origin: cfg.PublicBaseURL, Listen: cfg.Listen, Phase: "installing"}); err != nil {
			return err
		}
	}
	if !installed {
		fmt.Fprintln(m.Out, "Installing Caddy from your system's package repositories. Its dedicated gateway service will manage HTTPS certificates and renewal.")
		for _, command := range [][]string{{"apt-get", "update"}, {"apt-get", "install", "--yes", "caddy"}} {
			if err := m.runSetupInteractive(ctx, command...); err != nil {
				return err
			}
		}
		// Only this branch installed Caddy, after proving no previous Caddy
		// configuration or service existed. Preserve its packaged Caddyfile.
		if _, err := m.command(ctx, "systemctl", "disable", "--now", "caddy.service"); err != nil {
			return errors.New("could not stop the newly installed default Caddy service")
		}
		if !paths.PortsAvailable() {
			return errors.New("ports 80 or 443 became occupied; leave the existing server running and configure HTTPS manually")
		}
		if err := AtomicWrite(paths.Config, caddyfile, 0644, nil); err != nil {
			return err
		}
		if err := AtomicWrite(paths.Unit, unit, 0644, nil); err != nil {
			return err
		}
	}
	if _, err := m.command(ctx, "/usr/bin/caddy", "validate", "--config", paths.Config, "--adapter", "caddyfile"); err != nil {
		return errors.New("Caddy could not validate the gateway HTTPS configuration")
	}
	if _, err := m.command(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	// Starting the service can succeed before this process is interrupted.
	// Persist the configured boundary first, so a retry never mistakes our
	// own listener for a newly installed conflicting web server.
	if err := WriteJSON(paths.State, gatewayHTTPSState{Origin: cfg.PublicBaseURL, Listen: cfg.Listen, Phase: "configured"}); err != nil {
		return err
	}
	if _, err := m.command(ctx, "systemctl", "enable", "--now", gatewayProxyService); err != nil {
		return errors.New("the gateway HTTPS service could not start; inspect sudo journalctl -u codex-gateway-proxy")
	}
	return WriteJSON(paths.State, gatewayHTTPSState{Origin: cfg.PublicBaseURL, Listen: cfg.Listen, Phase: "ready"})
}

func gatewayHTTPSDebian(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && key == "ID" {
			value = strings.Trim(value, `"'`)
			return value == "debian" || value == "ubuntu"
		}
	}
	return false
}

func gatewayAutomaticHTTPSOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil || !strings.Contains(host, ".") || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) > 63 || !regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`).MatchString(label) {
			return false
		}
	}
	return true
}

func gatewayCaddySite(cfg config.GatewayConfig) string {
	return fmt.Sprintf("%s {\n    @gateway path /tgadmin /tgadmin/* /tgapi/* /tghealthz /tgreadyz\n    handle @gateway {\n        reverse_proxy %s\n    }\n    handle {\n        respond 404\n    }\n}\n", strconv.Quote(cfg.PublicBaseURL), strconv.Quote(cfg.Listen))
}

func gatewayCaddyUnit(configuration string) string {
	return fmt.Sprintf(`[Unit]
Description=Codex Telegram Gateway HTTPS proxy
After=network-online.target codex-gateway.service
Wants=network-online.target

[Service]
Type=notify
User=caddy
Group=caddy
Environment=HOME=/var/lib/caddy
ExecStart=/usr/bin/caddy run --config %s --adapter caddyfile
Restart=on-failure
RestartSec=5
TimeoutStopSec=5
LimitNOFILE=1048576
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true

[Install]
WantedBy=multi-user.target
`, strconv.Quote(strings.ReplaceAll(configuration, "%", "%%")))
}

func gatewayHTTPSPortsAvailable() bool {
	var listeners []net.Listener
	defer func() {
		for _, listener := range listeners {
			listener.Close()
		}
	}()
	for _, address := range []string{":80", ":443"} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return false
		}
		listeners = append(listeners, listener)
	}
	return true
}
