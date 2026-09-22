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
					if node.name == "location" && strings.Contains(strings.Join(node.args, " "), "/tgapi/") {
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
