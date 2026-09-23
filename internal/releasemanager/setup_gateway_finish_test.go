package releasemanager

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/config"
)

type gatewaySetupRoundTrip func(*http.Request) (*http.Response, error)

func (f gatewaySetupRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func readyGatewayResponse(request *http.Request) (*http.Response, error) {
	status, body := http.StatusOK, ""
	switch request.URL.Path {
	case "/tgw/readyz":
		body = "ready\n"
	case "/tgw/admin/":
		body = "<html>CODEX GATEWAY</html>"
	case "/tgw/api/v1/workers/connect":
		status, body = http.StatusUnauthorized, "unauthorized\n"
	default:
		status = http.StatusNotFound
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func TestGatewayPublicSetupChecksTLSAndEveryRoute(t *testing.T) {
	var paths []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		response, _ := readyGatewayResponse(r)
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
		io.Copy(w, response.Body)
	}))
	defer server.Close()
	m := New(nil)
	if err := m.gatewayPublicReady(context.Background(), server.URL); err == nil {
		t.Fatal("an untrusted HTTPS certificate was accepted")
	}
	m.HTTP = server.Client()
	if err := m.gatewayPublicReady(context.Background(), server.URL); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"/tgw/readyz", "/tgw/admin/", "/tgw/api/v1/workers/connect"}) {
		t.Fatalf("checked routes = %v", paths)
	}
}

func TestGatewayPublicSetupRejectsMisconfiguredProxyAndRedirect(t *testing.T) {
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/tgw/readyz", "not ready\n", 200}, {"/tgw/readyz", "ready\n", 503},
		{"/tgw/admin/", "unrelated page", 200}, {"/tgw/api/v1/workers/connect", "", 404},
		{"/tgw/readyz", "", 302},
	} {
		t.Run(tc.path+tc.body, func(t *testing.T) {
			m := New(nil)
			m.HTTP.Transport = gatewaySetupRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "gateway.example.com" {
					t.Fatal("followed a readiness redirect")
				}
				response, _ := readyGatewayResponse(r)
				if r.URL.Path == tc.path {
					response.StatusCode, response.Body = tc.status, io.NopCloser(strings.NewReader(tc.body))
					response.Header.Set("Location", "https://unrelated.example.com")
				}
				return response, nil
			})
			if err := m.gatewayPublicReady(context.Background(), "https://gateway.example.com"); err == nil {
				t.Fatal("accepted a misconfigured public proxy")
			}
		})
	}
}

func TestFinishGatewayRegistersBotAsServiceAccountAndGuidesEnrollment(t *testing.T) {
	m, l, cwd := gatewaySetupFixture(t)
	path, _ := preparedGatewayConfig(t, cwd, l)
	cfg, err := config.LoadGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Binary = "/usr/local/lib/codex-telegramgw/codex-gateway"
	var output bytes.Buffer
	m.Out = &output
	m.HTTP.Transport = gatewaySetupRoundTrip(readyGatewayResponse)
	var commands [][]string
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		if filepath.Base(args[0]) == "nginx" || (len(args) > 1 && args[0] == "systemctl" && args[1] == "show") {
			return CommandResult{ExitCode: 1}, nil
		}
		if len(args) == 2 && args[1] == "--help" {
			return CommandResult{Output: []byte("admin bootstrap-if-needed")}, nil
		}
		commands = append(commands, args)
		return CommandResult{Output: []byte("success\n")}, nil
	}
	prompt := &scriptedWorkerSetup{t: t}
	if err := m.guideGatewayCompletion(context.Background(), l, cfg, prompt); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 3 {
		t.Fatalf("commands = %v", commands)
	}
	for i, command := range commands {
		if !reflect.DeepEqual(command[:8], []string{"systemd-run", "--quiet", "--wait", "--pipe", "--collect", "--uid=codexgateway", "--gid=codexgateway", "-p"}) || command[8] != "EnvironmentFile="+l.Environment {
			t.Fatalf("administration did not load the private service environment: %v", command)
		}
		want := [][]string{{"menu", "set"}, {"webhook", "set"}, {"admin", "bootstrap-if-needed"}}[i]
		if !reflect.DeepEqual(command[len(command)-2:], want) {
			t.Fatalf("unexpected operation %v", command)
		}
	}
	for _, message := range []string{"https://gateway.example.com/tgw/admin/", "enroll a worker", "worker ID", "curl -fsSL", "/tgsessions"} {
		if !strings.Contains(output.String(), message) {
			t.Fatalf("missing setup guidance: %s", message)
		}
	}
}

func TestFinishGatewayCanRetryHTTPSOrDeferWithoutTelegramChanges(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "defer", true: "retry"}[retry], func(t *testing.T) {
			m, l, cwd := gatewaySetupFixture(t)
			path, _ := preparedGatewayConfig(t, cwd, l)
			cfg, _ := config.LoadGateway(path)
			var output bytes.Buffer
			m.Out = &output
			checks, commands := 0, 0
			m.HTTP.Transport = gatewaySetupRoundTrip(func(r *http.Request) (*http.Response, error) {
				checks++
				if !retry || checks < 3 {
					return nil, errors.New("certificate pending")
				}
				return readyGatewayResponse(r)
			})
			m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
				if filepath.Base(args[0]) == "nginx" || (len(args) > 1 && args[0] == "systemctl" && args[1] == "show") {
					return CommandResult{ExitCode: 1}, nil
				}
				if len(args) == 2 && args[1] == "--help" {
					return CommandResult{Output: []byte("admin bootstrap-if-needed")}, nil
				}
				commands++
				return CommandResult{}, nil
			}
			answers := []string{"later"}
			if retry {
				answers = []string{"manual", "check", "check"}
			}
			prompt := &scriptedWorkerSetup{t: t, answers: answers}
			if err := m.guideGatewayCompletion(context.Background(), l, cfg, prompt); err != nil {
				t.Fatal(err)
			}
			wantCommands := 0
			if retry {
				wantCommands = 3
			}
			if commands != wantCommands || !strings.Contains(output.String(), "sudo codex-telegramgw finish gateway") {
				t.Fatal("HTTPS retry/defer did not preserve resumable setup")
			}
		})
	}
}

func TestFinishGatewayCommandFailureDoesNotClaimCompletion(t *testing.T) {
	m, l, cwd := gatewaySetupFixture(t)
	path, _ := preparedGatewayConfig(t, cwd, l)
	cfg, _ := config.LoadGateway(path)
	var output bytes.Buffer
	m.Out = &output
	m.HTTP.Transport = gatewaySetupRoundTrip(readyGatewayResponse)
	m.Run = func(context.Context, ...string) (CommandResult, error) {
		return CommandResult{ExitCode: 1, Output: []byte("private-token must not escape")}, nil
	}
	if err := m.guideGatewayCompletion(context.Background(), l, cfg, &scriptedWorkerSetup{t: t}); err == nil {
		t.Fatal("failed Telegram registration was treated as complete")
	}
	if strings.Contains(output.String(), "private-token") || strings.Contains(output.String(), "enroll a worker") || !strings.Contains(output.String(), "finish gateway") {
		t.Fatal("failed setup leaked command output or omitted resume guidance")
	}
}

func TestFinishGatewayOlderBinaryKeepsAdministratorSetupPending(t *testing.T) {
	m, l, cwd := gatewaySetupFixture(t)
	path, _ := preparedGatewayConfig(t, cwd, l)
	cfg, _ := config.LoadGateway(path)
	var output bytes.Buffer
	m.Out = &output
	m.HTTP.Transport = gatewaySetupRoundTrip(readyGatewayResponse)
	commands := 0
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		if filepath.Base(args[0]) == "nginx" || (len(args) > 1 && args[0] == "systemctl" && args[1] == "show") {
			return CommandResult{ExitCode: 1}, nil
		}
		if len(args) == 2 && args[1] == "--help" {
			return CommandResult{Output: []byte("serve|admin bootstrap|menu set|webhook set")}, nil
		}
		if args[len(args)-2] == "admin" {
			t.Fatal("older gateway was asked to rotate its administrator bootstrap token")
		}
		commands++
		return CommandResult{}, nil
	}
	if err := m.guideGatewayCompletion(context.Background(), l, cfg, &scriptedWorkerSetup{t: t}); err != nil {
		t.Fatal(err)
	}
	if commands != 2 || !strings.Contains(output.String(), "administrator setup is pending") || !strings.Contains(output.String(), "sudo codex-telegramgw update gateway") || !strings.Contains(output.String(), "finish gateway") || strings.Contains(output.String(), "one-time token above") {
		t.Fatal("older gateway was not given accurate update/resume guidance")
	}
}
