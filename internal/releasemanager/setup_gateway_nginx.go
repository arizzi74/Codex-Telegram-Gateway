package releasemanager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/config"
)

type gatewayNginxPaths struct {
	Binary, SnippetsDir string
}

func defaultGatewayNginxPaths() gatewayNginxPaths {
	binary, _ := exec.LookPath("nginx")
	if binary == "" {
		for _, path := range []string{"/usr/sbin/nginx", "/usr/local/sbin/nginx", "/usr/local/nginx/sbin/nginx"} {
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				binary = path
				break
			}
		}
	}
	return gatewayNginxPaths{Binary: binary, SnippetsDir: "/etc/codex-gateway/nginx"}
}

// A host is one active HTTP server block, not one line in a configuration
// directory. The snapshot lets installation reject changes made since selection.
type gatewayNginxHost struct {
	Names, Ports  []string
	File          string
	block         *gatewayNginxDirective
	directives    []*gatewayNginxDirective
	configuration *gatewayNginxConfiguration
}

func (host gatewayNginxHost) Origins() []string {
	var origins []string
	for _, name := range host.Names {
		if !gatewayNginxConcreteName(name) {
			continue
		}
		for _, port := range host.Ports {
			address := name
			if port != "443" {
				address = net.JoinHostPort(name, port)
			}
			origins = append(origins, "https://"+address)
		}
	}
	return origins
}

func gatewayNginxConcreteName(name string) bool {
	if name == "" || name == "_" || strings.ContainsAny(name, "*~$\\/: \t\r\n") || strings.HasPrefix(name, ".") {
		return false
	}
	u, err := config.ParseHTTPSOrigin("https://" + name)
	return err == nil && u.Hostname() == name
}

func (m *Manager) discoverGatewayNginx(ctx context.Context, paths gatewayNginxPaths) ([]gatewayNginxHost, error) {
	if paths.Binary == "" {
		return nil, nil
	}
	service, err := m.Run(ctx, "systemctl", "show", "nginx.service", "--property=ExecStart", "--value")
	if err != nil || service.ExitCode != 0 || !gatewayNginxDefaultService(service.Output, paths.Binary) {
		return nil, errors.New("nginx uses a custom or unavailable service command; guided virtual-host setup supports the standard nginx systemd service, so choose standalone/manual HTTPS for this installation")
	}
	result, err := m.Run(ctx, paths.Binary, "-T")
	if err != nil || result.ExitCode != 0 {
		// nginx output may include unrelated secrets from other virtual hosts.
		return nil, errors.New("nginx is installed but its active configuration could not be inspected; run sudo nginx -t to inspect it, or choose standalone/manual HTTPS")
	}
	configuration, err := readGatewayNginxConfiguration(result.Output)
	if err != nil {
		return nil, err
	}
	return configuration.hosts()
}

// Do not inspect nginx's default tree and then reload a service which uses a
// different -c/-p configuration or wrapper. Only the standard direct service
// invocation (and its harmless daemon/master_process flags) is automated.
func gatewayNginxDefaultService(output []byte, binary string) bool {
	match := regexp.MustCompile(`path=([^ ;]+) ; argv\[\]=(.+?) ;`).FindSubmatch(output)
	if len(match) != 3 {
		return false
	}
	serviceBinary := string(match[1])
	if resolved, err := filepath.EvalSymlinks(serviceBinary); err == nil {
		serviceBinary = resolved
	}
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		binary = resolved
	}
	if serviceBinary != binary {
		return false
	}
	command := string(match[2])
	program := string(match[1])
	if command == program {
		return true
	}
	if !strings.HasPrefix(command, program+" -g ") {
		return false
	}
	flags := strings.TrimSpace(strings.TrimPrefix(command, program+" -g "))
	flags = strings.Trim(flags, "\"'")
	return regexp.MustCompile(`^(?:(?:daemon|master_process)\s+(?:on|off);\s*)+$`).MatchString(flags)
}

// setupGatewayNginx writes one managed include and inserts it in the chosen
// server. It never rewrites the website's existing directives or certificates.
func (m *Manager) setupGatewayNginx(ctx context.Context, cfg config.GatewayConfig, host gatewayNginxHost, paths gatewayNginxPaths) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if host.configuration == nil || host.block == nil || paths.Binary == "" {
		return errors.New("select an inspected nginx HTTPS virtual host before installing gateway routes")
	}
	if !host.matchesOrigin(cfg.PublicBaseURL) {
		return errors.New("gateway HTTPS address must match a server name and TLS port of the selected nginx virtual host; wildcard names need a matching concrete hostname")
	}
	listenHost, listenPort, err := net.SplitHostPort(cfg.Listen)
	if err != nil || (listenHost != "127.0.0.1" && listenHost != "::1" && listenHost != "localhost") {
		return errors.New("nginx gateway routes require a loopback gateway listen address")
	}
	if port, err := strconv.Atoi(listenPort); err != nil || port < 1 || port > 65535 {
		return errors.New("invalid local gateway listen port")
	}
	if !filepath.IsAbs(paths.SnippetsDir) || strings.ContainsAny(paths.SnippetsDir, "\n\r$;") {
		return errors.New("nginx managed snippet directory must be a safe absolute path")
	}
	// An include can be shared by several sites. Inserting into the actual
	// server block keeps the change confined to the selection.
	target, err := filepath.EvalSymlinks(host.File)
	if err != nil || !regularNoSymlink(target) {
		return errors.New("selected nginx virtual host must resolve to a regular configuration file")
	}
	if err := host.configuration.unchanged(); err != nil {
		return err
	}
	id := sha256.Sum256([]byte(target + ":" + strconv.Itoa(host.block.start)))
	snippet := filepath.Join(paths.SnippetsDir, fmt.Sprintf("codex-gateway-%x.conf", id[:8]))
	var existingInclude *gatewayNginxDirective
	for _, directive := range host.block.children {
		if directive.name == "include" && len(directive.args) == 1 {
			path := host.configuration.includePattern(directive.args[0])
			if filepath.Dir(path) == filepath.Clean(paths.SnippetsDir) && strings.HasPrefix(filepath.Base(path), "codex-gateway-") && !strings.ContainsAny(path, "*?[") {
				if existingInclude != nil {
					return errors.New("the selected nginx virtual host contains multiple managed gateway includes; review it manually")
				}
				existingInclude, snippet = directive, path
			}
		}
	}
	expected := []byte(gatewayNginxSnippet(cfg))
	for _, directive := range host.directives {
		if existingInclude != nil && filepath.Clean(directive.file) == filepath.Clean(snippet) {
			continue
		}
		if directive.name == "return" || directive.name == "rewrite" || directive.name == "if" {
			return errors.New("the selected nginx virtual host has server-level redirects or rewrites; configure its gateway routes manually to preserve the existing website")
		}
		conflict, err := host.configuration.routeConflict(directive, map[string]bool{})
		if err != nil {
			return err
		}
		if conflict {
			return errors.New("the selected nginx virtual host already configures gateway paths; refusing to overwrite existing routes")
		}
	}
	if existingInclude != nil {
		actual, err := os.ReadFile(snippet)
		if err != nil || !regularNoSymlink(snippet) || !bytes.Equal(actual, expected) {
			return errors.New("managed nginx gateway routes have changed; review them manually before repeating setup")
		}
		if err := m.validateGatewayNginx(ctx, paths); err != nil {
			return err
		}
		return m.reloadGatewayNginx(ctx)
	}
	if FileExists(snippet) {
		return errors.New("the managed nginx gateway snippet already exists without its expected include; refusing to overwrite it")
	}
	if info, err := os.Lstat(paths.SnippetsDir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("nginx managed snippet directory must be a real directory")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(paths.SnippetsDir, 0755); err != nil {
		return err
	}
	if err := os.Chmod(paths.SnippetsDir, 0755); err != nil {
		return err
	}
	original := host.configuration.files[filepath.Clean(host.File)].data
	// Backups are private and outside nginx's active include directories.
	backup, err := os.CreateTemp(paths.SnippetsDir, "virtual-host-backup-*.bak")
	if err != nil {
		return err
	}
	backupName := backup.Name()
	if _, err = backup.Write(original); err == nil {
		err = backup.Sync()
	}
	closeErr := backup.Close()
	if err != nil || closeErr != nil {
		os.Remove(backupName)
		return errors.New("could not save the nginx virtual host backup")
	}
	if err := host.configuration.unchanged(); err != nil {
		return err
	}
	if err := AtomicWrite(snippet, expected, 0644, nil); err != nil {
		return err
	}
	// Keep a final comparison immediately before the only pre-existing file
	// mutation. If another administrator edits a file, require rediscovery.
	if err := host.configuration.unchanged(); err != nil {
		os.Remove(snippet)
		return err
	}
	include := []byte("\n    # Gateway routes managed by codex-telegramgw setup.\n    include " + strconv.Quote(snippet) + ";\n")
	updated := append([]byte(nil), original[:host.block.end]...)
	updated = append(updated, include...)
	updated = append(updated, original[host.block.end:]...)
	if err := AtomicWrite(target, updated, 0644, nil); err != nil {
		os.Remove(snippet)
		return err
	}
	rollback := func(cause error, reload bool) error {
		current, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(current, updated) {
			return fmt.Errorf("%w; configuration changed during setup, so automatic rollback was skipped; restore backup %s", cause, backupName)
		}
		if err := AtomicWrite(target, original, 0644, nil); err != nil {
			return fmt.Errorf("%w; restore nginx virtual host backup %s", cause, backupName)
		}
		os.Remove(snippet)
		if reload {
			// The caller may have cancelled just as nginx accepted the reload.
			// Restore the old configuration with an independent bounded context.
			recovery, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := m.validateGatewayNginx(recovery, paths); err != nil {
				return fmt.Errorf("%w; original files restored but nginx validation failed", cause)
			}
			if err := m.reloadGatewayNginx(recovery); err != nil {
				return fmt.Errorf("%w; original files restored but nginx could not be reloaded", cause)
			}
		}
		return cause
	}
	if err := m.validateGatewayNginx(ctx, paths); err != nil {
		return rollback(err, false)
	}
	if err := m.reloadGatewayNginx(ctx); err != nil {
		return rollback(err, true)
	}
	fmt.Fprintf(m.Out, "Gateway routes added to the selected nginx HTTPS virtual host. Its original configuration is backed up at %s.\n", backupName)
	return nil
}

func (m *Manager) validateGatewayNginx(ctx context.Context, paths gatewayNginxPaths) error {
	result, err := m.Run(ctx, paths.Binary, "-t")
	if err != nil || result.ExitCode != 0 {
		return errors.New("nginx rejected the gateway routes; its original virtual host configuration was preserved")
	}
	return nil
}

func (m *Manager) reloadGatewayNginx(ctx context.Context) error {
	result, err := m.Run(ctx, "systemctl", "reload", "nginx.service")
	if err != nil || result.ExitCode != 0 {
		return errors.New("nginx could not reload the gateway routes")
	}
	return nil
}

func gatewayNginxSnippet(cfg config.GatewayConfig) string {
	upstream := "http://" + cfg.Listen
	const headers = "    proxy_set_header Host $http_host;\n    proxy_set_header X-Real-IP $remote_addr;\n    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n    proxy_set_header X-Forwarded-Proto $scheme;\n"
	var out strings.Builder
	out.WriteString("# Managed by codex-telegramgw setup; gateway routes only.\n")
	out.WriteString("location = /tgadmin { return 308 /tgadmin/; }\n")
	for _, location := range []string{"^~ /tgadmin/", "= /tgapi/v1/workers/connect", "^~ /tgapi/", "= /tghealthz", "= /tgreadyz"} {
		fmt.Fprintf(&out, "location %s {\n    proxy_pass %s;\n    proxy_http_version 1.1;\n", location, upstream)
		out.WriteString(headers)
		if strings.Contains(location, "workers/connect") {
			out.WriteString("    proxy_set_header Upgrade $http_upgrade;\n    proxy_set_header Connection \"upgrade\";\n    proxy_read_timeout 75s;\n    proxy_send_timeout 75s;\n")
		} else {
			out.WriteString("    proxy_set_header Connection \"\";\n    proxy_read_timeout 30s;\n")
		}
		out.WriteString("}\n")
	}
	return out.String()
}

func (host gatewayNginxHost) matchesOrigin(origin string) bool {
	u, err := config.ParseHTTPSOrigin(origin)
	if err != nil {
		return false
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	if !slices.Contains(host.Ports, port) {
		return false
	}
	name := strings.ToLower(u.Hostname())
	for _, pattern := range host.Names {
		pattern = strings.ToLower(pattern)
		if gatewayNginxConcreteName(pattern) && name == pattern {
			return true
		}
		if strings.HasPrefix(pattern, "*.") && strings.HasSuffix(name, pattern[1:]) && len(name) > len(pattern)-1 {
			return true
		}
		if strings.HasSuffix(pattern, ".*") && strings.HasPrefix(name, pattern[:len(pattern)-1]) && len(name) > len(pattern)-1 {
			return true
		}
		if strings.HasPrefix(pattern, ".") && (name == pattern[1:] || strings.HasSuffix(name, pattern)) {
			return true
		}
	}
	return false
}

func (host gatewayNginxHost) MatchesOrigin(origin string) bool { return host.matchesOrigin(origin) }

func (configuration *gatewayNginxConfiguration) routeConflict(directive *gatewayNginxDirective, seen map[string]bool) (bool, error) {
	if gatewayNginxRouteConflict(directive) {
		return true, nil
	}
	children, err := configuration.expanded(directive.children, seen)
	if err != nil {
		return false, err
	}
	for _, child := range children {
		if conflict, err := configuration.routeConflict(child, seen); conflict || err != nil {
			return conflict, err
		}
	}
	return false, nil
}

func gatewayNginxRouteConflict(directive *gatewayNginxDirective) bool {
	if directive.name == "location" {
		for _, arg := range directive.args {
			for _, path := range []string{"/tgadmin", "/tgapi", "/tghealthz", "/tgreadyz"} {
				if strings.Contains(arg, path) || (len(arg) > 1 && strings.HasPrefix(arg, "/") && strings.HasPrefix(path, arg)) {
					return true
				}
			}
		}
	}
	for _, child := range directive.children {
		if gatewayNginxRouteConflict(child) {
			return true
		}
	}
	return false
}

type gatewayNginxDirective struct {
	name, file string
	args       []string
	children   []*gatewayNginxDirective
	start, end int
}

type gatewayNginxFile struct {
	data  []byte
	path  string // Resolved path, so a changed sites-enabled symlink is detected.
	nodes []*gatewayNginxDirective
}

type gatewayNginxConfiguration struct {
	root     string
	files    map[string]*gatewayNginxFile
	patterns map[string][]string
}

func readGatewayNginxConfiguration(dump []byte) (*gatewayNginxConfiguration, error) {
	if len(dump) > 16*1024*1024 {
		return nil, errors.New("nginx configuration dump is too large for guided setup")
	}
	headers := regexp.MustCompile(`(?m)^# configuration file (.+):\r?$`).FindAllSubmatchIndex(dump, -1)
	if len(headers) == 0 {
		return nil, errors.New("nginx did not return an inspectable active configuration")
	}
	configuration := &gatewayNginxConfiguration{files: map[string]*gatewayNginxFile{}, patterns: map[string][]string{}}
	for index, header := range headers {
		path := filepath.Clean(string(dump[header[2]:header[3]]))
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n") {
			return nil, errors.New("nginx returned an unsupported configuration filename")
		}
		if configuration.root == "" {
			configuration.root = path
		}
		if configuration.files[path] != nil {
			return nil, errors.New("nginx configuration contains ambiguous file dump markers")
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !regularNoSymlink(resolved) {
			return nil, errors.New("nginx configuration file could not be read safely")
		}
		data, err := os.ReadFile(resolved)
		if err != nil || len(data) > 4*1024*1024 {
			return nil, errors.New("nginx configuration file could not be read within the setup size limit")
		}
		// nginx emits each loaded file verbatim after its marker. Compare to
		// disk, so a change during the inspection cannot silently be adopted.
		// Every dumped file is followed by exactly one newline, including a
		// source file which already ended in a newline. Check the whole
		// section rather than accepting a stale prefix after a concurrent edit.
		end := len(dump)
		if index+1 < len(headers) {
			end = headers[index+1][0]
		}
		start := header[1] + 1
		if header[1] >= len(dump) || dump[header[1]] != '\n' || end <= start || dump[end-1] != '\n' || !bytes.Equal(dump[start:end-1], data) {
			return nil, errors.New("nginx configuration changed during inspection; retry selection")
		}
		nodes, err := parseGatewayNginx(data, path)
		if err != nil {
			return nil, errors.New("nginx configuration uses syntax that guided setup cannot safely edit; choose manual HTTPS")
		}
		configuration.files[path] = &gatewayNginxFile{data: data, path: resolved, nodes: nodes}
	}
	return configuration, nil
}

func (configuration *gatewayNginxConfiguration) includePattern(value string) string {
	if !filepath.IsAbs(value) {
		value = filepath.Join(filepath.Dir(configuration.root), value)
	}
	return filepath.Clean(value)
}

func (configuration *gatewayNginxConfiguration) expanded(nodes []*gatewayNginxDirective, seen map[string]bool) ([]*gatewayNginxDirective, error) {
	var result []*gatewayNginxDirective
	for _, node := range nodes {
		if node.name != "include" {
			result = append(result, node)
			continue
		}
		if len(node.args) != 1 {
			return nil, errors.New("nginx include could not be safely inspected")
		}
		pattern := configuration.includePattern(node.args[0])
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, errors.New("nginx include pattern could not be safely inspected")
		}
		configuration.patterns[pattern] = append([]string(nil), matches...)
		for _, path := range matches {
			file := configuration.files[path]
			if file == nil || seen[path] {
				return nil, errors.New("nginx include graph changed or could not be safely inspected")
			}
			seen[path] = true
			children, err := configuration.expanded(file.nodes, seen)
			delete(seen, path)
			if err != nil {
				return nil, err
			}
			result = append(result, children...)
		}
	}
	return result, nil
}

func (configuration *gatewayNginxConfiguration) hosts() ([]gatewayNginxHost, error) {
	root, err := configuration.expanded(configuration.files[configuration.root].nodes, map[string]bool{configuration.root: true})
	if err != nil {
		return nil, err
	}
	var hosts []gatewayNginxHost
	for _, node := range root {
		if node.name != "http" {
			continue
		}
		httpNodes, err := configuration.expanded(node.children, map[string]bool{})
		if err != nil {
			return nil, err
		}
		inheritedCertificate := gatewayNginxHasDirective(httpNodes, "ssl_certificate")
		for _, server := range httpNodes {
			if server.name != "server" || server.children == nil {
				continue
			}
			directives, err := configuration.expanded(server.children, map[string]bool{})
			if err != nil {
				return nil, err
			}
			if !inheritedCertificate && !gatewayNginxHasDirective(directives, "ssl_certificate") {
				continue
			}
			host := gatewayNginxHost{File: server.file, block: server, directives: directives, configuration: configuration}
			reject := false
			for _, directive := range directives {
				switch directive.name {
				case "server_name":
					host.Names = append(host.Names, directive.args...)
				case "ssl_reject_handshake":
					reject = slices.Contains(directive.args, "on")
				case "listen":
					if len(directive.args) < 2 || !slices.Contains(directive.args[1:], "ssl") || slices.Contains(directive.args[1:], "quic") {
						continue
					}
					port := gatewayNginxListenPort(directive.args[0])
					if port != "" && !slices.Contains(host.Ports, port) {
						host.Ports = append(host.Ports, port)
					}
				}
			}
			if len(host.Ports) > 0 && !reject {
				host.Names = slices.Compact(host.Names)
				hosts = append(hosts, host)
			}
		}
	}
	return hosts, nil
}

func gatewayNginxHasDirective(nodes []*gatewayNginxDirective, name string) bool {
	for _, node := range nodes {
		if node.name == name && len(node.args) > 0 {
			return true
		}
	}
	return false
}

func gatewayNginxListenPort(value string) string {
	if value == "*" || net.ParseIP(strings.Trim(value, "[]")) != nil {
		// nginx defaults an address-only HTTP listener to port 80, even
		// when that listener explicitly enables TLS.
		return "80"
	}
	if !strings.ContainsAny(value, ":.") {
		port, err := strconv.Atoi(value)
		if err == nil && port > 0 && port < 65536 {
			return strconv.Itoa(port)
		}
		return ""
	}
	_, port, err := net.SplitHostPort(value)
	if err != nil {
		if gatewayNginxConcreteName(value) && strings.Contains(value, ".") {
			return "80"
		}
		return ""
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return ""
	}
	return strconv.Itoa(number)
}

func (configuration *gatewayNginxConfiguration) unchanged() error {
	for path, file := range configuration.files {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != file.path || !regularNoSymlink(resolved) {
			return errors.New("nginx configuration changed since selection; run setup again")
		}
		actual, err := os.ReadFile(resolved)
		if err != nil || !bytes.Equal(actual, file.data) {
			return errors.New("nginx configuration changed since selection; run setup again")
		}
	}
	for pattern, previous := range configuration.patterns {
		current, err := filepath.Glob(pattern)
		if err != nil || !slices.Equal(current, previous) {
			return errors.New("nginx active virtual hosts changed since selection; run setup again")
		}
	}
	return nil
}

// nginx has a small, whitespace-delimited configuration language. This lexer
// respects quoted/escaped characters, comments and ${variables}; offsets always
// point into the original bytes, so no unrelated directives are reserialized.
type gatewayNginxToken struct {
	value       string
	position    int
	punctuation bool
}

func parseGatewayNginx(data []byte, path string) ([]*gatewayNginxDirective, error) {
	tokens, err := lexGatewayNginx(data)
	if err != nil {
		return nil, err
	}
	position := 0
	var parse func(bool, int) ([]*gatewayNginxDirective, int, error)
	parse = func(nested bool, depth int) ([]*gatewayNginxDirective, int, error) {
		if depth > 128 {
			return nil, 0, errors.New("nginx nesting exceeds limit")
		}
		var nodes []*gatewayNginxDirective
		for position < len(tokens) {
			token := tokens[position]
			if token.punctuation {
				if token.value == "}" && nested {
					position++
					return nodes, token.position, nil
				}
				return nil, 0, errors.New("unexpected nginx delimiter")
			}
			position++
			node := &gatewayNginxDirective{name: token.value, start: token.position, file: path}
			for position < len(tokens) && !tokens[position].punctuation {
				node.args = append(node.args, tokens[position].value)
				position++
			}
			if position == len(tokens) {
				return nil, 0, errors.New("unterminated nginx directive")
			}
			delimiter := tokens[position]
			position++
			switch delimiter.value {
			case ";":
				node.end = delimiter.position
			case "{":
				var err error
				node.children, node.end, err = parse(true, depth+1)
				if err != nil {
					return nil, 0, err
				}
				if node.children == nil {
					node.children = []*gatewayNginxDirective{}
				}
			default:
				return nil, 0, errors.New("missing nginx directive delimiter")
			}
			nodes = append(nodes, node)
		}
		if nested {
			return nil, 0, errors.New("unterminated nginx block")
		}
		return nodes, len(data), nil
	}
	nodes, _, err := parse(false, 0)
	return nodes, err
}

func lexGatewayNginx(data []byte) ([]gatewayNginxToken, error) {
	var tokens []gatewayNginxToken
	for i := 0; i < len(data); {
		if strings.ContainsRune(" \t\r\n", rune(data[i])) {
			i++
			continue
		}
		if data[i] == '#' {
			for i < len(data) && data[i] != '\n' {
				i++
			}
			continue
		}
		if strings.ContainsRune("{};", rune(data[i])) {
			tokens = append(tokens, gatewayNginxToken{value: string(data[i]), position: i, punctuation: true})
			i++
			continue
		}
		start := i
		var word strings.Builder
		var quote byte
		for i < len(data) {
			c := data[i]
			if c == '\\' {
				i++
				if i == len(data) {
					return nil, errors.New("trailing nginx escape")
				}
				switch data[i] {
				case 'n':
					word.WriteByte('\n')
				case 'r':
					word.WriteByte('\r')
				case 't':
					word.WriteByte('\t')
				default:
					word.WriteByte(data[i])
				}
				i++
				continue
			}
			if quote != 0 {
				if c == quote {
					quote = 0
				} else {
					word.WriteByte(c)
				}
				i++
				continue
			}
			if c == '\'' || c == '"' {
				quote = c
				i++
				continue
			}
			if c == '$' && i+1 < len(data) && data[i+1] == '{' {
				end := bytes.IndexByte(data[i+2:], '}')
				if end < 0 {
					return nil, errors.New("unterminated nginx variable")
				}
				word.Write(data[i : i+end+3])
				i += end + 3
				continue
			}
			if strings.ContainsRune(" \t\r\n{};", rune(c)) {
				break
			}
			word.WriteByte(c)
			i++
		}
		if quote != 0 {
			return nil, errors.New("unterminated nginx quote")
		}
		tokens = append(tokens, gatewayNginxToken{value: word.String(), position: start})
	}
	return tokens, nil
}
