package releasemanager

import (
	"bytes"
	"context"
	"crypto/x509"
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
	"time"

	"github.com/iaia/telegramgw/internal/config"
)

const gatewayProxyService = "codex-gateway-proxy.service"

type gatewayHTTPSPaths struct {
	OSRelease, Config, Unit, State string
	ExistingCaddyPaths             []string
	PortAvailable                  func(int) bool
	CaddyAvailable                 func() bool
	PackageManagerAvailable        func() bool
	CertificateRoots               *x509.CertPool // nil uses the operating system's trusted roots.
}

func defaultGatewayHTTPSPaths() gatewayHTTPSPaths {
	return gatewayHTTPSPaths{
		OSRelease: "/etc/os-release", Config: "/etc/codex-gateway-proxy/Caddyfile",
		Unit: "/etc/systemd/system/" + gatewayProxyService, State: "/etc/codex-gateway-proxy/setup.json",
		ExistingCaddyPaths:      []string{"/etc/caddy", "/etc/systemd/system/caddy.service", "/etc/systemd/system/caddy-api.service"},
		PortAvailable:           gatewayHTTPSPortAvailable,
		CaddyAvailable:          func() bool { return regularNoSymlink("/usr/bin/caddy") },
		PackageManagerAvailable: func() bool { _, err := exec.LookPath("apt-get"); return err == nil },
	}
}

// Empty certificate paths request automatic public certificate issuance and
// renewal. Supplied PEM files are copied to this proxy's protected directory;
// the administrator renews the source files externally; certificate refresh
// verifies and loads renewed copies into this dedicated proxy.
type gatewayHTTPSOptions struct {
	CertificateFile string `json:"certificate_file,omitempty"`
	KeyFile         string `json:"key_file,omitempty"`
	TLSALPNOnly     bool   `json:"tls_alpn_only,omitempty"`
}

type gatewayHTTPSState struct {
	Origin         string              `json:"origin"`
	Listen         string              `json:"listen"`
	Phase          string              `json:"phase"`
	Options        gatewayHTTPSOptions `json:"options,omitempty"`
	InstallPackage bool                `json:"install_package"`
}

func (m *Manager) setupGatewayHTTPS(ctx context.Context, cfg config.GatewayConfig, paths gatewayHTTPSPaths) error {
	return m.setupGatewayHTTPSWithOptions(ctx, cfg, paths, gatewayHTTPSOptions{})
}

// setupGatewayHTTPSWithOptions manages a dedicated Caddy service without
// changing other web servers, their configuration, or their service state.
func (m *Manager) setupGatewayHTTPSWithOptions(ctx context.Context, cfg config.GatewayConfig, paths gatewayHTTPSPaths, options gatewayHTTPSOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, input := range []*string{&options.CertificateFile, &options.KeyFile} {
		if *input != "" {
			absolute, err := filepath.Abs(*input)
			if err != nil {
				return errors.New("certificate paths could not be resolved")
			}
			*input = absolute
		}
	}
	if !gatewayStandaloneHTTPSOrigin(cfg.PublicBaseURL) {
		return errors.New("standalone HTTPS requires a public DNS hostname and Telegram webhook port 443, 80, 88, or 8443")
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil || port == "" || (host != "127.0.0.1" && host != "::1" && host != "localhost") {
		return errors.New("standalone HTTPS requires the gateway to listen on a loopback address")
	}
	origin, _ := url.Parse(cfg.PublicBaseURL)
	httpsPort := 443
	if origin.Port() != "" {
		httpsPort, _ = strconv.Atoi(origin.Port())
	}
	state := gatewayHTTPSState{Origin: cfg.PublicBaseURL, Listen: cfg.Listen, Phase: "installing", Options: options}
	managed, installed := false, false
	if data, err := os.ReadFile(paths.State); err == nil {
		if !privateRegularSetupFile(paths.State) || json.Unmarshal(data, &state) != nil || state.Origin != cfg.PublicBaseURL || state.Listen != cfg.Listen || (state.Phase != "installing" && state.Phase != "configured" && state.Phase != "ready") {
			return errors.New("managed HTTPS settings have changed; review the proxy configuration manually")
		}
		if options == (gatewayHTTPSOptions{}) {
			options = state.Options
		}
		if options != state.Options {
			return errors.New("managed HTTPS certificate settings have changed; review the proxy configuration manually")
		}
		managed = true
		installed = state.Phase != "installing"
		// Older setup state predates InstallPackage. A partially installed
		// automatic proxy from that version owns its package installation.
		if !installed && !bytes.Contains(data, []byte(`"install_package"`)) {
			state.InstallPackage = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("could not read managed HTTPS setup state")
	}
	certificateMode := options.CertificateFile != "" || options.KeyFile != ""
	if !managed && !certificateMode && httpsPort == 443 && !paths.PortAvailable(80) {
		options.TLSALPNOnly = true
		state.Options = options
	}
	var certificate, key []byte
	certificatePath, keyPath := gatewayHTTPSCertificatePaths(paths)
	if certificateMode {
		if options.CertificateFile == "" || options.KeyFile == "" {
			return errors.New("supply both the public certificate chain and its private key")
		}
		readOptions := options
		if installed {
			readOptions = gatewayHTTPSOptions{CertificateFile: certificatePath, KeyFile: keyPath}
		}
		certificate, key, err = readGatewayHTTPSCertificate(readOptions, origin.Hostname(), paths.CertificateRoots)
		if err != nil {
			return err
		}
	} else if httpsPort == 80 {
		return errors.New("HTTPS on port 80 requires an existing public certificate and private key; automatic certificate validation also needs port 80")
	}
	caddyfile := []byte(gatewayManagedCaddyfile(cfg, paths, certificateMode, options.TLSALPNOnly))
	unit := []byte(gatewayCaddyUnit(paths.Config))
	if managed {
		for path, expected := range map[string][]byte{paths.Config: caddyfile, paths.Unit: unit} {
			actual, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) && state.Phase == "installing" && !FileExists(path) {
				continue
			}
			if err != nil || !regularNoSymlink(path) || !bytes.Equal(actual, expected) {
				return errors.New("managed HTTPS files have changed; refusing to replace them")
			}
		}
	}
	checkPorts := func() error {
		if !paths.PortAvailable(httpsPort) {
			return fmt.Errorf("HTTPS port %d is already in use; select another port or use an existing nginx virtual host", httpsPort)
		}
		if !certificateMode && !options.TLSALPNOnly && !paths.PortAvailable(80) {
			return errors.New("automatic certificates require unused port 80 reachable from the Internet; select an existing nginx virtual host or provide a public certificate and private key")
		}
		return nil
	}
	if !managed {
		for _, path := range []string{paths.Config, filepath.Dir(paths.Config), paths.Unit} {
			if FileExists(path) {
				return errors.New("an existing dedicated proxy configuration was found; review it manually before continuing")
			}
		}
		state.InstallPackage = !paths.CaddyAvailable()
		if state.InstallPackage {
			data, err := os.ReadFile(paths.OSRelease)
			if err != nil || !gatewayHTTPSDebian(data) || !paths.PackageManagerAvailable() {
				return errors.New("installing Caddy automatically requires Debian or Ubuntu; install /usr/bin/caddy first or use an existing HTTPS reverse proxy")
			}
			for _, path := range paths.ExistingCaddyPaths {
				if FileExists(path) {
					return errors.New("Caddy configuration exists but /usr/bin/caddy is unavailable; repair that installation before continuing")
				}
			}
			for _, service := range []string{"caddy.service", "caddy-api.service"} {
				result, err := m.Run(ctx, "systemctl", "is-active", "--quiet", service)
				if err != nil || result.ExitCode == 0 {
					return errors.New("an existing Caddy service may be running without /usr/bin/caddy; repair that installation before continuing")
				}
			}
		}
		if err := checkPorts(); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(paths.Config), 0755); err != nil {
			return err
		}
		if err := os.Chmod(filepath.Dir(paths.Config), 0755); err != nil {
			return err
		}
		if err := WriteJSON(paths.State, state); err != nil {
			return err
		}
	}
	if !installed {
		if state.InstallPackage {
			fmt.Fprintln(m.Out, "Installing Caddy from your system's package repositories for a dedicated gateway proxy service.")
			if err := m.installGatewayCaddy(ctx); err != nil {
				return err
			}
		}
		if err := checkPorts(); err != nil {
			return err
		}
		if certificateMode {
			if err := m.writeGatewayHTTPSCertificate(ctx, paths, certificate, key); err != nil {
				return err
			}
		}
		if err := AtomicWrite(paths.Config, caddyfile, 0644, nil); err != nil {
			return err
		}
		if err := AtomicWrite(paths.Unit, unit, 0644, nil); err != nil {
			return err
		}
	}
	// Run validation as the service user too: root-only certificate paths must
	// never produce a seemingly successful installation that cannot start.
	if _, err := m.command(ctx, "runuser", "-u", "caddy", "--", "/usr/bin/caddy", "validate", "--config", paths.Config, "--adapter", "caddyfile"); err != nil {
		return errors.New("Caddy could not validate or read the gateway HTTPS configuration and certificates")
	}
	if _, err := m.command(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	state.Phase = "configured"
	if err := WriteJSON(paths.State, state); err != nil {
		return err
	}
	if _, err := m.command(ctx, "systemctl", "enable", "--now", gatewayProxyService); err != nil {
		return errors.New("the gateway HTTPS service could not start; inspect sudo journalctl -u codex-gateway-proxy")
	}
	state.Phase = "ready"
	if err := WriteJSON(paths.State, state); err != nil {
		return err
	}
	if certificateMode {
		fmt.Fprintf(m.Out, "This proxy uses copied certificates. Renew them externally, replace %s and %s (root:caddy, mode 0640), or run sudo codex-telegramgw https refresh after your certificate provider renews them.\n", certificatePath, keyPath)
	}
	return nil
}

func (m *Manager) installGatewayCaddy(ctx context.Context) (resultErr error) {
	// Only a newly owned package installation enters this method. Mask its
	// packaged service before apt so it cannot bind ports used by nginx or an
	// unrelated proxy. Never stop or disable a previously installed Caddy.
	if _, err := m.command(ctx, "systemctl", "mask", "caddy.service"); err != nil {
		return errors.New("could not prevent the newly installed Caddy package from starting its default service")
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := m.command(cleanupCtx, "systemctl", "unmask", "caddy.service"); err != nil {
			if resultErr == nil {
				resultErr = errors.New("could not remove the temporary Caddy package service mask")
			}
			return
		}
		// disable while masked is ignored by systemd. Unmask first, then
		// remove any autostart links the package post-install created.
		if _, err := m.command(cleanupCtx, "systemctl", "disable", "caddy.service"); err != nil && resultErr == nil {
			resultErr = errors.New("could not disable the newly installed default Caddy service")
		}
	}()
	for _, command := range [][]string{{"apt-get", "update"}, {"apt-get", "install", "--yes", "caddy"}} {
		if err := m.runSetupInteractive(ctx, command...); err != nil {
			return err
		}
	}
	return nil
}

func gatewayManagedCaddyfile(cfg config.GatewayConfig, paths gatewayHTTPSPaths, certificateMode, tlsALPNOnly bool) string {
	globals := "    admin off\n"
	site := gatewayCaddySite(cfg)
	if certificateMode {
		globals += "    auto_https off\n"
		// Caddy rejects an HTTPS address on its configured plaintext HTTP
		// port. Automatic HTTPS is off, so this convention override does
		// not create a listener on 8081.
		if origin, _ := url.Parse(cfg.PublicBaseURL); origin.Port() == "80" {
			globals += "    http_port 8081\n"
		}
		cert, key := gatewayHTTPSCertificatePaths(paths)
		site = strings.Replace(site, " {\n", " {\n    tls "+strconv.Quote(cert)+" "+strconv.Quote(key)+"\n", 1)
	} else if tlsALPNOnly {
		globals += "    auto_https disable_redirects\n"
		site = strings.Replace(site, " {\n", " {\n    tls {\n        issuer acme {\n            disable_http_challenge\n        }\n    }\n", 1)
	} else if origin, _ := url.Parse(cfg.PublicBaseURL); origin.Port() != "" && origin.Port() != "443" {
		globals += "    auto_https disable_redirects\n"
		site = strings.Replace(site, " {\n", " {\n    tls {\n        issuer acme {\n            disable_tlsalpn_challenge\n        }\n    }\n", 1)
	}
	return "# Managed by codex-telegramgw setup.\n{\n" + globals + "}\n\n" + site
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
	return err == nil && (u.Port() == "" || u.Port() == "443") && gatewayStandaloneHTTPSOrigin(origin)
}

func gatewayStandaloneHTTPSOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || !gatewayHTTPSWebhookPort(u.Port()) {
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
	return fmt.Sprintf("%s {\n    @gateway path /tgadmin /tgadmin/* /tgapi/* /tghealthz /tgreadyz\n    handle @gateway {\n        reverse_proxy %s {\n            header_up X-Real-IP {remote_host}\n        }\n    }\n    handle {\n        respond 404\n    }\n}\n", strconv.Quote(cfg.PublicBaseURL), strconv.Quote(cfg.Listen))
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

func gatewayHTTPSWebhookPort(port string) bool {
	switch port {
	case "", "443", "80", "88", "8443":
		return true
	}
	return false
}

func gatewayHTTPSPortAvailable(port int) bool {
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	listener.Close()
	return true
}
