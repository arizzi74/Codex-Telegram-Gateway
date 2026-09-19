package releasemanager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/config"
)

type gatewayNginxFixture struct {
	manager                  *Manager
	paths                    gatewayNginxPaths
	main, site, enabled, tls string
	commands                 [][]string
}

func newGatewayNginxFixture(t *testing.T) *gatewayNginxFixture {
	t.Helper()
	root := t.TempDir()
	f := &gatewayNginxFixture{
		manager: New(io.Discard),
		paths:   gatewayNginxPaths{Binary: "/test/nginx", SnippetsDir: filepath.Join(root, "managed")},
		main:    filepath.Join(root, "nginx.conf"),
		site:    filepath.Join(root, "sites-available/website"),
		enabled: filepath.Join(root, "sites-enabled/website"),
		tls:     filepath.Join(root, "snippets/tls.conf"),
	}
	platformWrite(t, f.main, `events {}
http {
    ssl_certificate /test/global-cert.pem;
    ssl_certificate_key /test/global-key.pem;
    include "sites-enabled/*";
}
stream { server { listen 443 ssl; server_name stream.example.com; } }
`, 0644)
	platformWrite(t, f.site, `# The HTTP redirect should not appear among TLS sites.
server { listen 80; server_name website.example.com; return 301 https://$host$request_uri; }
server {
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name website.example.com alias.example.com;
    include snippets/tls.conf;
    set $example "literal { } # ; and \"quotes\"";
    location / {
        add_header X-Quoted 'nested } {;#';
        return 200 "original website ${host}";
    }
}
server {
    listen 8443 ssl;
    server_name other.example.com;
    location / { return 200 "other website"; }
}
server { listen 443 ssl; server_name _; ssl_reject_handshake on; }
`, 0640)
	platformWrite(t, f.tls, "listen 8443 ssl;\nssl_certificate /test/site-cert.pem;\nssl_certificate_key /test/site-key.pem;\n", 0644)
	if err := os.MkdirAll(filepath.Dir(f.enabled), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.site, f.enabled); err != nil {
		t.Fatal(err)
	}
	f.manager.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		f.commands = append(f.commands, append([]string(nil), args...))
		if len(args) > 1 && args[1] == "show" {
			return CommandResult{Output: []byte("{ path=" + f.paths.Binary + " ; argv[]=" + f.paths.Binary + " -g daemon on; master_process on; ; ignore_errors=no ; }")}, nil
		}
		if reflect.DeepEqual(args, []string{f.paths.Binary, "-T"}) {
			return CommandResult{Output: f.dump(t)}, nil
		}
		return CommandResult{}, nil
	}
	return f
}

func (f *gatewayNginxFixture) dump(t *testing.T) []byte {
	t.Helper()
	var dump bytes.Buffer
	dump.WriteString("nginx: configuration syntax is ok\nnginx: configuration test is successful\n")
	files := []string{f.main, f.enabled, f.tls}
	managed, err := filepath.Glob(filepath.Join(f.paths.SnippetsDir, "codex-gateway-*.conf"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, managed...)
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&dump, "# configuration file %s:\n", path)
		dump.Write(data)
		dump.WriteByte('\n')
	}
	return dump.Bytes()
}

func (f *gatewayNginxFixture) hosts(t *testing.T) []gatewayNginxHost {
	t.Helper()
	hosts, err := f.manager.discoverGatewayNginx(context.Background(), f.paths)
	if err != nil {
		t.Fatal(err)
	}
	return hosts
}

func TestGatewayNginxDiscoveryFollowsActiveHTTPIncludesAndTLSInheritance(t *testing.T) {
	f := newGatewayNginxFixture(t)
	hosts := f.hosts(t)
	if len(hosts) != 2 {
		t.Fatalf("TLS virtual hosts = %d", len(hosts))
	}
	if !reflect.DeepEqual(hosts[0].Names, []string{"website.example.com", "alias.example.com"}) || !reflect.DeepEqual(hosts[0].Ports, []string{"443", "8443"}) {
		t.Fatalf("first host = %+v", hosts[0])
	}
	if hosts[0].File != f.enabled {
		t.Fatal("site source was not retained")
	}
	if got := hosts[1].Origins(); !reflect.DeepEqual(got, []string{"https://other.example.com:8443"}) {
		t.Fatalf("inherited certificate host = %v", got)
	}
	if !hosts[0].MatchesOrigin("https://alias.example.com:8443") || hosts[0].MatchesOrigin("https://other.example.com") || hosts[0].MatchesOrigin("https://website.example.com:4433") {
		t.Fatal("origin validation accepted a different host or port")
	}
}

func TestGatewayNginxInstallPreservesSitesSymlinksAndIsIdempotent(t *testing.T) {
	f := newGatewayNginxFixture(t)
	host := f.hosts(t)[0]
	original, _ := os.ReadFile(f.site)
	cfg := config.GatewayConfig{PublicBaseURL: "https://website.example.com", Listen: "127.0.0.1:8080"}
	if err := f.manager.setupGatewayNginx(context.Background(), cfg, host, f.paths); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(f.enabled)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("sites-enabled symlink was replaced", err)
	}
	info, _ = os.Stat(f.site)
	if info.Mode().Perm() != 0640 {
		t.Fatalf("site permissions = %v", info.Mode())
	}
	updated, _ := os.ReadFile(f.site)
	snippets, _ := filepath.Glob(filepath.Join(f.paths.SnippetsDir, "codex-gateway-*.conf"))
	if len(snippets) != 1 {
		t.Fatalf("managed snippets = %v", snippets)
	}
	include := "\n    # Gateway routes managed by codex-telegramgw setup.\n    include " + fmt.Sprintf("%q", snippets[0]) + ";\n"
	if strings.Replace(string(updated), include, "", 1) != string(original) {
		t.Fatal("unrelated website bytes changed")
	}
	if !bytes.Contains(updated[:host.block.end+len(include)], []byte(include)) {
		t.Fatal("include was not inserted into selected server")
	}
	backups, _ := filepath.Glob(filepath.Join(f.paths.SnippetsDir, "virtual-host-backup-*.bak"))
	if len(backups) != 1 {
		t.Fatal("site backup was not created")
	}
	backup, _ := os.ReadFile(backups[0])
	info, _ = os.Stat(backups[0])
	if !bytes.Equal(backup, original) || info.Mode().Perm() != 0600 {
		t.Fatal("backup changed original bytes or exposed private configuration")
	}
	proxy, _ := os.ReadFile(snippets[0])
	for _, want := range []string{"location ^~ /tgadmin/", "location ^~ /tgapi/", "location = /tgreadyz", "location = /tghealthz", "location = /tgapi/v1/workers/connect", `proxy_set_header Connection "upgrade"`, "proxy_pass http://127.0.0.1:8080;", "proxy_set_header Host $http_host;"} {
		if !bytes.Contains(proxy, []byte(want)) {
			t.Fatalf("missing proxy configuration %q", want)
		}
	}
	if bytes.Contains(proxy, []byte("$connection_upgrade")) {
		t.Fatal("snippet requires a global map in an unrelated configuration")
	}
	host = f.hosts(t)[0]
	if err := f.manager.setupGatewayNginx(context.Background(), cfg, host, f.paths); err != nil {
		t.Fatal("repeat setup failed", err)
	}
	again, _ := os.ReadFile(f.site)
	if !bytes.Equal(again, updated) {
		t.Fatal("repeat setup duplicated the include")
	}
	backups, _ = filepath.Glob(filepath.Join(f.paths.SnippetsDir, "virtual-host-backup-*.bak"))
	if len(backups) != 1 {
		t.Fatal("repeat setup changed existing files")
	}
}

func TestGatewayNginxRollbackOnValidationOrReloadFailure(t *testing.T) {
	for _, phase := range []string{"validation", "reload"} {
		t.Run(phase, func(t *testing.T) {
			f := newGatewayNginxFixture(t)
			host := f.hosts(t)[0]
			original, _ := os.ReadFile(f.site)
			run := f.manager.Run
			reloads, validations := 0, 0
			f.manager.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
				if len(args) > 1 && args[1] == "-t" {
					validations++
					if phase == "validation" {
						return CommandResult{ExitCode: 1, Output: []byte("private nginx details")}, nil
					}
				}
				if len(args) > 1 && args[1] == "reload" {
					reloads++
					if phase == "reload" && reloads == 1 {
						return CommandResult{}, errors.New("private nginx details")
					}
				}
				return run(ctx, args...)
			}
			cfg := config.GatewayConfig{PublicBaseURL: "https://website.example.com", Listen: "127.0.0.1:8080"}
			err := f.manager.setupGatewayNginx(context.Background(), cfg, host, f.paths)
			if err == nil || strings.Contains(err.Error(), "private nginx details") {
				t.Fatal("failure was swallowed or private output exposed", err)
			}
			after, _ := os.ReadFile(f.site)
			if !bytes.Equal(after, original) {
				t.Fatal("failed setup did not restore original virtual host")
			}
			files, _ := filepath.Glob(filepath.Join(f.paths.SnippetsDir, "codex-gateway-*.conf"))
			if len(files) != 0 {
				t.Fatal("failed setup left active snippet")
			}
			if phase == "validation" && (reloads != 0 || validations != 1) {
				t.Fatal("invalid configuration was reloaded")
			}
			if phase == "reload" && (reloads != 2 || validations != 2) {
				t.Fatal("uncertain reload was not recovered")
			}
		})
	}
}

func TestGatewayNginxRefusesConcurrentChangesAndConflictingRoutes(t *testing.T) {
	for _, scenario := range []string{"changed-site", "changed-symlink", "new-active-site", "existing-route", "included-route", "nested-included-route", "server-rewrite", "wrong-origin", "nonloopback", "changed-managed-snippet"} {
		t.Run(scenario, func(t *testing.T) {
			f := newGatewayNginxFixture(t)
			if scenario == "existing-route" || scenario == "included-route" || scenario == "nested-included-route" || scenario == "server-rewrite" {
				path := f.tls
				addition := "location = /tgapi/v1/telegram/webhook { return 200; }\n"
				if scenario == "existing-route" {
					path = f.site
					addition = "\nserver { listen 443 ssl; server_name conflict.example.com; location /tgadmin/ { return 200; } }\n"
				}
				if scenario == "nested-included-route" {
					platformWrite(t, f.tls, "location / { include snippets/nested.conf; }\n", 0644)
					path = filepath.Join(filepath.Dir(f.tls), "nested.conf")
				}
				if scenario == "server-rewrite" {
					addition = "return 301 https://other.example.com$request_uri;\n"
				}
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString(addition); err != nil {
					t.Fatal(err)
				}
				file.Close()
				if scenario == "nested-included-route" {
					run := f.manager.Run
					f.manager.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
						result, err := run(ctx, args...)
						if len(args) > 1 && args[1] == "-T" {
							result.Output = append(result.Output, []byte(fmt.Sprintf("# configuration file %s:\n%s\n", path, addition))...)
						}
						return result, err
					}
				}
			}
			hosts := f.hosts(t)
			host := hosts[0]
			cfg := config.GatewayConfig{PublicBaseURL: "https://website.example.com", Listen: "127.0.0.1:8080"}
			if scenario == "existing-route" {
				host = hosts[len(hosts)-1]
				cfg.PublicBaseURL = "https://conflict.example.com"
			}
			switch scenario {
			case "changed-site":
				platformWrite(t, f.site, "# concurrent site change\n", 0644)
			case "changed-symlink":
				data, _ := os.ReadFile(f.site)
				alternate := f.site + "-copy"
				platformWrite(t, alternate, string(data), 0640)
				os.Remove(f.enabled)
				if err := os.Symlink(alternate, f.enabled); err != nil {
					t.Fatal(err)
				}
			case "new-active-site":
				platformWrite(t, filepath.Join(filepath.Dir(f.enabled), "newsite"), "server { listen 80; }\n", 0644)
			case "wrong-origin":
				cfg.PublicBaseURL = "https://unrelated.example.com"
			case "nonloopback":
				cfg.Listen = "0.0.0.0:8080"
			case "changed-managed-snippet":
				if err := f.manager.setupGatewayNginx(context.Background(), cfg, host, f.paths); err != nil {
					t.Fatal(err)
				}
				paths, _ := filepath.Glob(filepath.Join(f.paths.SnippetsDir, "codex-gateway-*.conf"))
				file, _ := os.OpenFile(paths[0], os.O_APPEND|os.O_WRONLY, 0644)
				file.WriteString("# administrator customization\n")
				file.Close()
				host = f.hosts(t)[0]
			}
			before, _ := os.ReadFile(f.site)
			f.commands = nil
			if err := f.manager.setupGatewayNginx(context.Background(), cfg, host, f.paths); err == nil {
				t.Fatal("unsafe setup accepted")
			}
			after, _ := os.ReadFile(f.site)
			if !bytes.Equal(before, after) || len(f.commands) != 0 {
				t.Fatal("rejected setup changed configuration or services")
			}
		})
	}
}

func TestGatewayNginxWildcardOriginAndNoSecretOutput(t *testing.T) {
	host := gatewayNginxHost{Names: []string{"*.example.com", "mail.*", ".example.net", "~^private"}, Ports: []string{"443", "8443"}}
	for _, origin := range []string{"https://a.example.com", "https://a.b.example.com:8443", "https://mail.example.org", "https://example.net", "https://a.example.net"} {
		if !host.MatchesOrigin(origin) {
			t.Fatalf("matching wildcard rejected: %s", origin)
		}
	}
	for _, origin := range []string{"https://example.com", "https://notexample.com", "https://example.org", "https://example.net:9443", "http://a.example.com", "https://a.example.com/path"} {
		if host.MatchesOrigin(origin) {
			t.Fatalf("unmatched wildcard accepted: %s", origin)
		}
	}
	if len(host.Origins()) != 0 {
		t.Fatal("wildcard host generated unusable URL suggestions")
	}
	m := New(io.Discard)
	m.Run = func(context.Context, ...string) (CommandResult, error) {
		return CommandResult{Output: []byte("secret upstream credential"), ExitCode: 1}, errors.New("secret upstream credential")
	}
	if _, err := m.discoverGatewayNginx(context.Background(), gatewayNginxPaths{Binary: "/test/nginx"}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("inspection failure leaked configuration", err)
	}
	if hosts, err := m.discoverGatewayNginx(context.Background(), gatewayNginxPaths{}); err != nil || len(hosts) != 0 {
		t.Fatal("absent nginx was not handled", err)
	}
}

func TestGatewayNginxDetectionUsesPATH(t *testing.T) {
	dir := t.TempDir()
	platformWrite(t, filepath.Join(dir, "nginx"), "#!/bin/sh\nexit 0\n", 0755)
	t.Setenv("PATH", dir)
	if paths := defaultGatewayNginxPaths(); paths.Binary != filepath.Join(dir, "nginx") {
		t.Fatalf("detected nginx = %q", paths.Binary)
	}
}

func TestGatewayNginxRejectsStaleDumpAndTracksTLSListenPorts(t *testing.T) {
	f := newGatewayNginxFixture(t)
	dump := f.dump(t)
	// A shortened file is still a prefix of the old dump, but it is not the
	// configuration which nginx inspected successfully.
	platformWrite(t, f.main, "events {}\n", 0644)
	if _, err := readGatewayNginxConfiguration(dump); err == nil {
		t.Fatal("stale dump accepted a concurrently shortened file")
	}
	for listener, expected := range map[string]string{
		"443": "443", "8443": "8443", "127.0.0.1:8443": "8443", "[::]:443": "443", "*:88": "88",
		"127.0.0.1": "80", "[::1]": "80", "*": "80", "gateway.example.com": "80",
		"0": "", "65536": "", "unix:/tmp/test.sock": "", "[::]:99999": "",
	} {
		if actual := gatewayNginxListenPort(listener); actual != expected {
			t.Fatalf("listen %s: port = %s; want %s", listener, actual, expected)
		}
	}
}

func TestGatewayNginxRefusesMismatchedServiceConfiguration(t *testing.T) {
	for _, command := range []string{
		"/test/nginx -c /different/nginx.conf", "/test/nginx -c/different/nginx.conf", "/test/nginx -p /other/root",
		"/test/nginx -g include /other/config;", "/test/nginx -g daemon on; -c /different/config",
	} {
		f := newGatewayNginxFixture(t)
		run := f.manager.Run
		f.manager.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
			if len(args) > 1 && args[1] == "show" {
				return CommandResult{Output: []byte("{ path=/test/nginx ; argv[]=" + command + " ; ignore_errors=no ; }")}, nil
			}
			return run(ctx, args...)
		}
		if _, err := f.manager.discoverGatewayNginx(context.Background(), f.paths); err == nil {
			t.Fatalf("custom service accepted: %s", command)
		}
		if len(f.commands) != 0 {
			t.Fatal("custom service's unrelated default nginx configuration was inspected")
		}
	}
	if gatewayNginxDefaultService([]byte("{ path=/test/wrapper ; argv[]=/test/wrapper ; ignore_errors=no ; }"), "/test/nginx") {
		t.Fatal("wrapper service accepted")
	}
	if !gatewayNginxDefaultService([]byte("{ path=/test/nginx ; argv[]=/test/nginx ; ignore_errors=no ; }"), "/test/nginx") {
		t.Fatal("plain nginx service rejected")
	}
}
