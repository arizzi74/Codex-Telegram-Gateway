package releasemanager

import (
	"os"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/config"
)

func TestGatewayProxyOverwritesClientIdentityOnAuthenticationRoutes(t *testing.T) {
	cfg := config.GatewayConfig{PublicBaseURL: "https://gateway.example.com", Listen: "127.0.0.1:8080"}
	generated := gatewayNginxSnippet(cfg)
	checked, err := os.ReadFile("../../deploy/nginx/telegramgw.conf")
	if err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]string{"generated": generated, "checked-in": string(checked)} {
		t.Run(name, func(t *testing.T) {
			nodes, err := parseGatewayNginx([]byte(source), name)
			if err != nil {
				t.Fatal(err)
			}
			var checked int
			var visit func([]*gatewayNginxDirective)
			visit = func(nodes []*gatewayNginxDirective) {
				for _, node := range nodes {
					if node.name == "location" && strings.Contains(strings.Join(node.args, " "), "/tgw/api/") {
						found := false
						for _, child := range node.children {
							if child.name == "proxy_set_header" && len(child.args) == 2 && child.args[0] == "X-Real-IP" && child.args[1] == "$remote_addr" {
								found = true
							}
						}
						if !found {
							t.Errorf("authentication route lacks overwritten client IP: %v", node.args)
						}
						checked++
					}
					visit(node.children)
				}
			}
			visit(nodes)
			if checked == 0 {
				t.Fatal("no gateway API locations checked")
			}
		})
	}
	if !strings.Contains(gatewayCaddySite(cfg), "header_up X-Real-IP {remote_host}") {
		t.Fatal("Caddy does not overwrite the trusted client header")
	}
}

func TestGatewayProxyUpgradesWorkerBrowserAndActivityConnections(t *testing.T) {
	checked, err := os.ReadFile("../../deploy/nginx/telegramgw.conf")
	if err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]string{"generated": gatewayNginxSnippet(config.GatewayConfig{Listen: "127.0.0.1:8080"}), "checked-in": string(checked)} {
		t.Run(name, func(t *testing.T) {
			nodes, err := parseGatewayNginx([]byte(source), name)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/tgw/api/v1/workers/connect", "/tgw/api/v1/webui/connect", "/tgw/api/v1/webui/activity"} {
				var exact, prefix *gatewayNginxDirective
				longest := -1
				var visit func([]*gatewayNginxDirective)
				visit = func(nodes []*gatewayNginxDirective) {
					for _, node := range nodes {
						if node.name == "location" {
							if len(node.args) == 2 && node.args[0] == "=" && node.args[1] == path {
								exact = node
							}
							var route string
							if len(node.args) == 1 {
								route = node.args[0]
							} else if len(node.args) == 2 && node.args[0] == "^~" {
								route = node.args[1]
							}
							if route != "" && strings.HasPrefix(path, route) && len(route) > longest {
								prefix, longest = node, len(route)
							}
						}
						visit(node.children)
					}
				}
				visit(nodes)
				matched := exact
				if matched == nil {
					matched = prefix
				}
				if matched == nil {
					t.Fatalf("no WebSocket location for %s", path)
				}
				headers := make(map[string]string)
				settings := make(map[string]string)
				for _, node := range matched.children {
					if node.name == "proxy_set_header" && len(node.args) == 2 {
						headers[node.args[0]] = node.args[1]
					} else {
						settings[node.name] = strings.Join(node.args, " ")
					}
				}
				connection := "upgrade"
				if name == "checked-in" {
					connection = "$telegramgw_connection_upgrade"
				}
				if headers["Upgrade"] != "$http_upgrade" || headers["Connection"] != connection || settings["proxy_http_version"] != "1.1" || settings["proxy_read_timeout"] != "75s" || settings["proxy_send_timeout"] != "75s" || settings["proxy_buffering"] != "off" {
					t.Fatalf("WebSocket route %s selected %v without required upgrade/live settings: headers=%v settings=%v", path, matched.args, headers, settings)
				}
			}
		})
	}
}
