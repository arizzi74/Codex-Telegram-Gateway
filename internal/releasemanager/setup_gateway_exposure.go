package releasemanager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/iaia/telegramgw/internal/config"
)

var errGatewayExposureBack = errors.New("return to HTTPS setup choices")

type gatewayExposurePlan struct {
	Mode, Origin string
	NginxHost    gatewayNginxHost
	NginxPaths   gatewayNginxPaths
	HTTPSOptions gatewayHTTPSOptions
}

// Only inspect active nginx configuration; directory listings also contain
// disabled sites and cannot establish which HTTPS hosts nginx actually serves.
func (m *Manager) inspectGatewayNginx(ctx context.Context) (gatewayNginxPaths, []gatewayNginxHost) {
	paths := defaultGatewayNginxPaths()
	if paths.Binary == "" {
		fmt.Fprintln(m.Out, "nginx is not installed.")
		return paths, nil
	}
	hosts, err := m.discoverGatewayNginx(ctx, paths)
	if err != nil {
		fmt.Fprintln(m.Out, err)
		return paths, nil
	}
	if len(hosts) == 0 {
		fmt.Fprintln(m.Out, "nginx is installed; no active HTTPS virtual hosts were found.")
	} else {
		fmt.Fprintln(m.Out, "nginx is installed. Active HTTPS virtual hosts:")
		for i, host := range hosts {
			fmt.Fprintf(m.Out, "  %d. %s — TLS ports %s\n     %s\n", i+1, strings.Join(host.Names, ", "), strings.Join(host.Ports, ", "), host.File)
		}
	}
	return paths, hosts
}

func gatewayTelegramPort(value string) (string, error) {
	switch strings.TrimSpace(value) {
	case "443", "80", "88", "8443":
		return strings.TrimSpace(value), nil
	default:
		return "", errors.New("Telegram webhooks require HTTPS port 443, 80, 88, or 8443")
	}
}

func gatewayTelegramOrigin(value string) (string, error) {
	origin, err := setupGatewayOrigin(value)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(origin)
	port := u.Port()
	if port == "" {
		port = "443"
	}
	if _, err := gatewayTelegramPort(port); err != nil {
		return "", err
	}
	return origin, nil
}

func gatewayNginxSuggestedOrigin(host gatewayNginxHost) string {
	for _, origin := range host.Origins() {
		if _, err := gatewayTelegramOrigin(origin); err == nil {
			return origin
		}
	}
	return ""
}

func gatewayNginxUsable(host gatewayNginxHost, origin string) bool {
	if origin != "" {
		_, err := gatewayTelegramOrigin(origin)
		return err == nil && host.MatchesOrigin(origin)
	}
	for _, port := range host.Ports {
		if _, err := gatewayTelegramPort(port); err != nil {
			continue
		}
		for _, name := range host.Names {
			// A default/regex-only site cannot establish a safe public origin.
			if name == "_" || strings.HasPrefix(name, "~") {
				continue
			}
			candidate := strings.ReplaceAll(name, "*", "gateway")
			if strings.HasPrefix(candidate, ".") {
				candidate = "gateway" + candidate
			}
			if host.MatchesOrigin("https://" + net.JoinHostPort(candidate, port)) {
				return true
			}
		}
	}
	return false
}

func (m *Manager) chooseGatewayNginx(ctx context.Context, prompt workerSetupPrompt, paths gatewayNginxPaths, hosts []gatewayNginxHost, origin string) (*gatewayExposurePlan, error) {
	fallback := ""
	for i, host := range hosts {
		if gatewayNginxUsable(host, origin) {
			fallback = strconv.Itoa(i + 1)
			break
		}
	}
	if fallback == "" {
		fmt.Fprintln(m.Out, "No listed nginx HTTPS virtual host matches the gateway address and a Telegram-supported port; use standalone or manual setup.")
		return nil, nil
	}
	selection, err := askSetupValue(ctx, prompt, m.Out, "nginx virtual host number (or back)", fallback, false, func(value string) (string, error) {
		if strings.EqualFold(strings.TrimSpace(value), "back") {
			return "back", nil
		}
		index, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || index < 1 || index > len(hosts) {
			return "", errors.New("select a virtual host number from the list")
		}
		if !gatewayNginxUsable(hosts[index-1], origin) {
			return "", errors.New("selected virtual host must match the configured HTTPS address and a Telegram-supported port")
		}
		return strconv.Itoa(index), nil
	})
	if err != nil || selection == "back" {
		return nil, err
	}
	index, _ := strconv.Atoi(selection)
	host := hosts[index-1]
	if origin == "" {
		fmt.Fprintln(m.Out, "Choose a hostname covered by this site's certificate. For wildcard sites, enter a concrete hostname.")
		origin, err = askSetupValue(ctx, prompt, m.Out, "Public gateway HTTPS address (or back)", gatewayNginxSuggestedOrigin(host), false, func(value string) (string, error) {
			if strings.EqualFold(strings.TrimSpace(value), "back") {
				return "back", nil
			}
			value, err := gatewayTelegramOrigin(value)
			if err != nil {
				return "", err
			}
			if !host.MatchesOrigin(value) {
				return "", errors.New("HTTPS address must match a server name and TLS port of the selected virtual host")
			}
			return value, nil
		})
		if err != nil || origin == "back" {
			return nil, err
		}
	}
	fmt.Fprintf(m.Out, "Gateway routes will be added to the selected site at %s. Other site routes will be preserved.\n", origin)
	return &gatewayExposurePlan{Mode: "nginx", Origin: origin, NginxHost: host, NginxPaths: paths}, nil
}

func (m *Manager) chooseGatewayCertificates(ctx context.Context, prompt workerSetupPrompt, origin string) (gatewayHTTPSOptions, error) {
	return m.chooseGatewayCertificatesWithPaths(ctx, prompt, origin, defaultGatewayHTTPSPaths())
}

func (m *Manager) chooseGatewayCertificatesWithPaths(ctx context.Context, prompt workerSetupPrompt, origin string, paths gatewayHTTPSPaths) (gatewayHTTPSOptions, error) {
	var options gatewayHTTPSOptions
	// Resuming a managed proxy uses its saved certificate mode and source paths.
	if FileExists(paths.State) {
		return options, nil
	}
	u, _ := url.Parse(origin)
	port := u.Port()
	if port == "" {
		port = "443"
	}
	fallback := "auto"
	if port == "80" || (port != "443" && !paths.PortAvailable(80)) {
		fallback = "files"
	}
	fmt.Fprintln(m.Out, "Certificates: auto obtains and renews a public certificate; files uses an existing public certificate chain and private key.")
	if port != "443" {
		fmt.Fprintln(m.Out, "Automatic certificates on an alternative HTTPS port need public port 80 available for validation. If it is occupied, choose files or an existing nginx site.")
	}
	mode, err := askSetupValue(ctx, prompt, m.Out, "Certificate setup (auto/files/back)", fallback, false, func(value string) (string, error) {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "back":
			return "back", nil
		case "auto":
			if port == "80" {
				return "", errors.New("HTTPS on port 80 requires existing certificate files")
			}
			if port != "443" && !paths.PortAvailable(80) {
				return "", errors.New("port 80 is occupied; choose files for an existing certificate, or back to select an nginx HTTPS site")
			}
			return "auto", nil
		case "files":
			return "files", nil
		default:
			return "", errors.New("enter auto, files, or back")
		}
	})
	if mode == "back" {
		return options, errGatewayExposureBack
	}
	if err != nil || mode == "auto" {
		return options, err
	}
	normalize := func(value string) (string, error) {
		value = strings.TrimSpace(value)
		if strings.EqualFold(value, "back") {
			return "back", nil
		}
		if !filepath.IsAbs(value) || strings.ContainsAny(value, "\r\n\x00") {
			return "", errors.New("enter an absolute path to a readable PEM file")
		}
		info, err := os.Stat(value)
		if err != nil || !info.Mode().IsRegular() {
			return "", errors.New("certificate or key file does not exist or is not a regular file")
		}
		return filepath.Clean(value), nil
	}
	options.CertificateFile, err = askSetupValue(ctx, prompt, m.Out, "Public certificate chain PEM file (or back)", "", false, normalize)
	if err != nil {
		return options, err
	}
	if options.CertificateFile == "back" {
		return options, errGatewayExposureBack
	}
	options.KeyFile, err = askSetupValue(ctx, prompt, m.Out, "Private key PEM file (or back)", "", false, normalize)
	if options.KeyFile == "back" {
		return options, errGatewayExposureBack
	}
	if err == nil {
		fmt.Fprintln(m.Out, "Renew the source certificate using your existing process. Gateway update checks refresh the proxy's private copies; a renewal hook can also run: sudo codex-telegramgw https refresh")
	}
	return options, err
}

func (m *Manager) chooseGatewayExposure(ctx context.Context, prompt workerSetupPrompt, paths gatewayNginxPaths, hosts []gatewayNginxHost) (*gatewayExposurePlan, error) {
	return m.chooseGatewayExposureWithHTTPSPaths(ctx, prompt, paths, hosts, defaultGatewayHTTPSPaths())
}

func (m *Manager) chooseGatewayExposureWithHTTPSPaths(ctx context.Context, prompt workerSetupPrompt, paths gatewayNginxPaths, hosts []gatewayNginxHost, httpsPaths gatewayHTTPSPaths) (*gatewayExposurePlan, error) {
	fallback := "standalone"
	for _, host := range hosts {
		if gatewayNginxUsable(host, "") {
			fallback = "nginx"
			break
		}
	}
	fmt.Fprintln(m.Out, "HTTPS setup: nginx adds gateway routes to an existing HTTPS site; standalone installs a dedicated Caddy proxy; manual lets you configure your own proxy.")
	for {
		mode, err := askSetupValue(ctx, prompt, m.Out, "HTTPS setup (nginx/standalone/manual)", fallback, false, func(value string) (string, error) {
			switch strings.ToLower(strings.TrimSpace(value)) {
			case "nginx":
				if len(hosts) == 0 {
					return "", errors.New("no active nginx HTTPS sites were found; choose standalone or manual")
				}
				return "nginx", nil
			case "standalone", "auto":
				return "standalone", nil
			case "manual":
				return "manual", nil
			default:
				return "", errors.New("enter nginx, standalone, or manual")
			}
		})
		if err != nil {
			return nil, err
		}
		if mode == "nginx" {
			plan, err := m.chooseGatewayNginx(ctx, prompt, paths, hosts, "")
			if err != nil {
				return nil, err
			}
			if plan == nil {
				continue
			}
			return plan, nil
		}
		origin, err := askSetupValue(ctx, prompt, m.Out, "Public gateway hostname or HTTPS address (or back)", "", false, func(value string) (string, error) {
			if strings.EqualFold(strings.TrimSpace(value), "back") {
				return "back", nil
			}
			origin, err := gatewayTelegramOrigin(value)
			if err == nil && mode == "standalone" && !gatewayStandaloneHTTPSOrigin(origin) {
				return "", errors.New("standalone HTTPS needs a public DNS hostname")
			}
			return origin, err
		})
		if err != nil {
			return nil, err
		}
		if origin == "back" {
			continue
		}
		plan := &gatewayExposurePlan{Mode: mode, Origin: origin}
		if mode == "manual" {
			return plan, nil
		}
		u, _ := url.Parse(origin)
		port := u.Port()
		if port == "" {
			port = "443"
			if paths.Binary != "" {
				port = "8443"
			}
		}
		port, err = askSetupValue(ctx, prompt, m.Out, "Public HTTPS port (443/80/88/8443, or back)", port, false, func(value string) (string, error) {
			if strings.EqualFold(strings.TrimSpace(value), "back") {
				return "back", nil
			}
			value, err := gatewayTelegramPort(value)
			if err != nil {
				return "", err
			}
			number, _ := strconv.Atoi(value)
			if !FileExists(httpsPaths.State) && !httpsPaths.PortAvailable(number) {
				return "", errors.New("that HTTPS port is already in use; choose another port or back to use an nginx site")
			}
			return value, nil
		})
		if err != nil {
			return nil, err
		}
		if port == "back" {
			continue
		}
		u.Host = u.Hostname()
		if port != "443" {
			u.Host = net.JoinHostPort(u.Hostname(), port)
		}
		plan.Origin = u.String()
		plan.HTTPSOptions, err = m.chooseGatewayCertificatesWithPaths(ctx, prompt, plan.Origin, httpsPaths)
		if errors.Is(err, errGatewayExposureBack) {
			continue
		}
		return plan, err
	}
}

func (m *Manager) applyGatewayExposure(ctx context.Context, cfg config.GatewayConfig, plan *gatewayExposurePlan) error {
	switch plan.Mode {
	case "nginx":
		return m.setupGatewayNginx(ctx, cfg, plan.NginxHost, plan.NginxPaths)
	case "standalone":
		if plan.HTTPSOptions.CertificateFile == "" {
			fmt.Fprintln(m.Out, "Caddy will request certificates from a public certificate authority; proceeding accepts its subscriber agreement.")
		}
		return m.setupGatewayHTTPSWithOptions(ctx, cfg, defaultGatewayHTTPSPaths(), plan.HTTPSOptions)
	}
	return nil
}
