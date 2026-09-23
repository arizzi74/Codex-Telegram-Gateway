package releasemanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/config"
)

// FinishGateway resumes HTTPS and Telegram setup without reinstalling or
// restarting the gateway and without replacing its configuration or secrets.
func (m *Manager) FinishGateway(ctx context.Context) error {
	l, err := NewLayout("gateway")
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("gateway setup requires sudo")
	}
	if err := l.RequireUser(); err != nil {
		return err
	}
	prompt, err := openWorkerSetupTerminal()
	if err != nil {
		return errors.New("finishing gateway setup needs an interactive terminal; run sudo codex-telegramgw finish gateway")
	}
	defer prompt.Close()
	return m.finishGateway(ctx, l, prompt)
}

func (m *Manager) finishGateway(ctx context.Context, l *Layout, prompt workerSetupPrompt) error {
	cfg, err := config.LoadGateway(l.Config)
	if err != nil {
		return errors.New("could not read installed gateway configuration; install the gateway first")
	}
	return m.guideGatewayCompletion(ctx, l, cfg, prompt)
}

func (m *Manager) gatewayFinishInstructions() {
	fmt.Fprintln(m.Out, "Resume HTTPS, Telegram, and administrator setup at any time:")
	fmt.Fprintln(m.Out, "  sudo codex-telegramgw finish gateway")
}

func (m *Manager) guideGatewayCompletion(ctx context.Context, l *Layout, cfg config.GatewayConfig, prompt workerSetupPrompt) error {
	fmt.Fprintf(m.Out, "\nFinish gateway setup for %s\n", cfg.PublicBaseURL)
	fmt.Fprintln(m.Out, "The domain's DNS records must point to this machine. Open the configured HTTPS port in the host and cloud firewalls; automatic certificates also require access to their validation port.")
	ready := m.gatewayPublicReady(ctx, cfg.PublicBaseURL) == nil
	if err := ctx.Err(); err != nil {
		return err
	}
	var paths gatewayNginxPaths
	var hosts []gatewayNginxHost
	fallback := "standalone"
	if !ready {
		paths, hosts = m.inspectGatewayNginx(ctx)
		for _, host := range hosts {
			if gatewayNginxUsable(host, cfg.PublicBaseURL) {
				fallback = "nginx"
				break
			}
		}
		fmt.Fprintln(m.Out, "Choose nginx to use an existing HTTPS site, standalone for a dedicated Caddy proxy, check to test your existing proxy, manual for instructions, or later to resume.")
		fmt.Fprintln(m.Out, "Finishing setup preserves the gateway's configured public address and administrator passkeys.")
	}
	for !ready {
		choice, err := prompt.Ask(ctx, "HTTPS setup (nginx/standalone/check/manual/later)", fallback, false)
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "later":
			m.gatewayFinishInstructions()
			return nil
		case "nginx", "standalone", "auto":
			var plan *gatewayExposurePlan
			if strings.EqualFold(strings.TrimSpace(choice), "nginx") {
				plan, err = m.chooseGatewayNginx(ctx, prompt, paths, hosts, cfg.PublicBaseURL)
			} else {
				plan = &gatewayExposurePlan{Mode: "standalone", Origin: cfg.PublicBaseURL}
				plan.HTTPSOptions, err = m.chooseGatewayCertificates(ctx, prompt, cfg.PublicBaseURL)
			}
			if errors.Is(err, errGatewayExposureBack) {
				continue
			}
			if err != nil {
				return err
			}
			if plan == nil {
				continue
			}
			fallback = "check"
			if err := m.applyGatewayExposure(ctx, cfg, plan); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				fmt.Fprintf(m.Out, "HTTPS setup could not finish: %s\n", err)
				m.printGatewayProxyInstructions(cfg)
				// Reload the snapshot before another attempt after a failed change.
				paths, hosts = m.inspectGatewayNginx(ctx)
				continue
			}
			fmt.Fprintln(m.Out, "HTTPS proxy configured. Certificate issuance can take a minute; checking the public gateway now.")
		case "manual":
			fallback = "check"
			m.printGatewayProxyInstructions(cfg)
			continue
		case "check":
		default:
			fmt.Fprintln(m.Out, "Enter nginx, standalone, check, manual, or later.")
			continue
		}
		if err := m.gatewayPublicReady(ctx, cfg.PublicBaseURL); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintln(m.Out, "Public HTTPS is not ready yet. Check DNS, the configured HTTPS port, certificate validation ports, and your proxy. For the managed proxy: sudo journalctl -u codex-gateway-proxy -n 30 --no-pager")
			fmt.Fprintln(m.Out, "Choose check to retry after making changes, or later to resume without changing this installation.")
			continue
		}
		break
	}
	fmt.Fprintln(m.Out, "Public HTTPS is ready. Registering the Telegram command menu and webhook.")
	for _, command := range [][]string{{"menu", "set"}, {"webhook", "set"}, {"admin", "bootstrap-if-needed"}} {
		if command[0] == "admin" {
			help, err := m.command(ctx, l.Binary, "--help")
			if err != nil {
				m.gatewayFinishInstructions()
				return errors.New("could not inspect the installed gateway's administrator setup support")
			}
			if !strings.Contains(string(help), "bootstrap-if-needed") {
				fmt.Fprintln(m.Out, "Telegram is configured. Guided administrator setup is pending because the installed gateway is older than this installer.")
				fmt.Fprintln(m.Out, "Update it when convenient, then resume setup:")
				fmt.Fprintln(m.Out, "  sudo codex-telegramgw update gateway")
				m.gatewayFinishInstructions()
				fmt.Fprintf(m.Out, "An existing administrator can continue signing in at %s/tgw/admin/.\n", cfg.PublicBaseURL)
				return nil
			}
		}
		output, err := m.gatewaySetupCommand(ctx, l, command...)
		if err != nil {
			m.gatewayFinishInstructions()
			return fmt.Errorf("gateway %s could not finish; check the bot settings and network connection, then resume setup: %w", strings.Join(command, " "), err)
		}
		fmt.Fprint(m.Out, string(output))
	}
	fmt.Fprintf(m.Out, "\nOpen %s/tgw/admin/ and register your passkey using the one-time token above, or sign in with your existing passkey.\n", cfg.PublicBaseURL)
	fmt.Fprintln(m.Out, "In the console, enroll a worker and copy its worker ID and one-time enrollment token. On the worker machine, run as the user who owns the projects:")
	fmt.Fprintf(m.Out, "  curl -fsSL https://raw.githubusercontent.com/%s/main/scripts/install.sh | sh\n", DefaultRepo)
	fmt.Fprintf(m.Out, "Enter %s when asked for the gateway address, then the worker ID and token.\n", cfg.PublicBaseURL)
	fmt.Fprintf(m.Out, "After the worker connects, open @%s in Telegram and use /tgsessions.\n", strings.TrimPrefix(cfg.Secrets.BotName, "@"))
	return nil
}

func (m *Manager) gatewaySetupCommand(ctx context.Context, l *Layout, args ...string) ([]byte, error) {
	root := l.DataRoot
	if root == "" {
		root = GatewayDataRoot
	}
	command := []string{"systemd-run", "--quiet", "--wait", "--pipe", "--collect", "--uid=codexgateway", "--gid=codexgateway",
		"-p", "EnvironmentFile=" + l.Environment, "-p", "WorkingDirectory=" + root, "-p", "UMask=0077",
		l.Binary, "--config", l.Config}
	return m.command(ctx, append(command, args...)...)
}

func (m *Manager) gatewayPublicReady(ctx context.Context, origin string) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	client := *m.HTTP
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, check := range []struct {
		path, marker string
		status       int
	}{
		{"/tgw/readyz", "ready\n", http.StatusOK},
		{"/tgw/admin/", "CODEX GATEWAY", http.StatusOK},
		{"/tgw/api/v1/workers/connect", "", http.StatusUnauthorized},
	} {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+check.path, nil)
		if err != nil {
			return errors.New("invalid public gateway address")
		}
		response, err := client.Do(request)
		if err != nil {
			return errors.New("public HTTPS could not be reached with a valid certificate")
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 512*1024))
		response.Body.Close()
		if readErr != nil || response.StatusCode != check.status || (check.marker != "" && !strings.Contains(string(body), check.marker)) || (check.path == "/tgw/readyz" && string(body) != "ready\n") {
			return errors.New("public HTTPS is not forwarding the gateway routes correctly")
		}
	}
	return nil
}

func (m *Manager) printGatewayProxyInstructions(cfg config.GatewayConfig) {
	fmt.Fprintf(m.Out, "\nForward the gateway routes to %s with WebSocket support, preserving their paths.\n", cfg.Listen)
	fmt.Fprintln(m.Out, "If you manage Caddy yourself, add this site to its configuration and reload Caddy:")
	fmt.Fprint(m.Out, gatewayCaddySite(cfg))
	fmt.Fprintln(m.Out, "  sudo systemctl reload caddy")
	fmt.Fprintf(m.Out, "For nginx, use https://github.com/%s/blob/main/deploy/nginx/telegramgw.conf with your domain, certificate, and local port.\n", DefaultRepo)
	fmt.Fprintf(m.Out, "The ready check is: curl -fsS %s/tgw/readyz\n", cfg.PublicBaseURL)
	m.gatewayFinishInstructions()
}
