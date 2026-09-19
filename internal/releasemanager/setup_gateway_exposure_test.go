package releasemanager

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayExposureSelectsNginxSiteAndRetriesWrongNames(t *testing.T) {
	f := newGatewayNginxFixture(t)
	hosts := f.hosts(t)
	var out bytes.Buffer
	f.manager.Out = &out
	prompt := &scriptedWorkerSetup{t: t, answers: []string{"nginx", "99", "1", "other.example.com:8443", "alias.example.com:8443"}}
	plan, err := f.manager.chooseGatewayExposureWithHTTPSPaths(context.Background(), prompt, f.paths, hosts, availableGatewayHTTPSPaths(t))
	if err != nil || plan == nil || plan.Mode != "nginx" || plan.Origin != "https://alias.example.com:8443" || plan.NginxHost.File != hosts[0].File || len(prompt.answers) != 0 {
		t.Fatalf("selection = %+v, %v", plan, err)
	}
	if strings.Count(out.String(), "Please try again.") != 2 {
		t.Fatal("invalid host/name not retried", out.String())
	}
}

func TestGatewayExposureStandalonePortBecomesPublicOrigin(t *testing.T) {
	for _, port := range []string{"443", "88", "8443"} {
		t.Run(port, func(t *testing.T) {
			m := New(nil)
			prompt := &scriptedWorkerSetup{t: t, answers: []string{"standalone", "gateway.example.com", "8080", port, "auto"}}
			plan, err := m.chooseGatewayExposureWithHTTPSPaths(context.Background(), prompt, gatewayNginxPaths{}, nil, availableGatewayHTTPSPaths(t))
			want := "https://gateway.example.com"
			if port != "443" {
				want += ":" + port
			}
			if err != nil || plan == nil || plan.Mode != "standalone" || plan.Origin != want || len(prompt.answers) != 0 {
				t.Fatalf("plan = %+v, %v", plan, err)
			}
		})
	}
}

func TestGatewayExposureBackAndCancelledPrompt(t *testing.T) {
	f := newGatewayNginxFixture(t)
	hosts := f.hosts(t)
	prompt := &scriptedWorkerSetup{t: t, answers: []string{"standalone", "gateway.example.com", "8443", "files", "back", "nginx", "2", ""}}
	plan, err := f.manager.chooseGatewayExposureWithHTTPSPaths(context.Background(), prompt, f.paths, hosts, availableGatewayHTTPSPaths(t))
	if err != nil || plan == nil || plan.Mode != "nginx" || plan.Origin != "https://other.example.com:8443" {
		t.Fatalf("back plan = %+v, %v", plan, err)
	}
	cancelled := &scriptedWorkerSetup{t: t, answers: []string{"nginx"}, endErr: io.EOF}
	if _, err := f.manager.chooseGatewayExposure(context.Background(), cancelled, f.paths, hosts); !errors.Is(err, io.EOF) {
		t.Fatal("cancelled selection was swallowed", err)
	}
}

func TestGatewayNginxChoicePreservesExistingOrigin(t *testing.T) {
	f := newGatewayNginxFixture(t)
	prompt := &scriptedWorkerSetup{t: t, answers: []string{"1", ""}}
	plan, err := f.manager.chooseGatewayNginx(context.Background(), prompt, f.paths, f.hosts(t), "https://other.example.com:8443")
	if err != nil || plan == nil || plan.Origin != "https://other.example.com:8443" || plan.NginxHost.Names[0] != "other.example.com" || len(prompt.answers) != 0 {
		t.Fatalf("existing origin selection = %+v, %v", plan, err)
	}
}

func TestGatewayNginxChoicesSkipUnusableNamesAndPorts(t *testing.T) {
	for _, tc := range []struct {
		names []string
		port  string
		want  bool
	}{
		{[]string{"_"}, "443", false},
		{[]string{"~^example\\.com$"}, "443", false},
		{[]string{"website.example.com"}, "8081", false},
		{[]string{"*.example.com"}, "443", true},
		{[]string{".example.com"}, "443", true},
		{[]string{"website.*"}, "8443", true},
		{[]string{"website.example.com"}, "443", true},
	} {
		host := gatewayNginxHost{Names: tc.names, Ports: []string{tc.port}}
		if got := gatewayNginxUsable(host, ""); got != tc.want {
			t.Fatalf("usable %v:%s = %v", tc.names, tc.port, got)
		}
	}
}

func TestGatewayExposureManualAndMissingNginx(t *testing.T) {
	m := New(nil)
	prompt := &scriptedWorkerSetup{t: t, answers: []string{"nginx", "manual", "gateway.example.com:8080", "gateway.example.com:8443"}}
	plan, err := m.chooseGatewayExposureWithHTTPSPaths(context.Background(), prompt, gatewayNginxPaths{}, nil, availableGatewayHTTPSPaths(t))
	if err != nil || plan == nil || plan.Mode != "manual" || plan.Origin != "https://gateway.example.com:8443" || len(prompt.answers) != 0 {
		t.Fatalf("manual plan = %+v, %v", plan, err)
	}
}

func availableGatewayHTTPSPaths(t *testing.T) gatewayHTTPSPaths {
	t.Helper()
	return gatewayHTTPSPaths{State: filepath.Join(t.TempDir(), "state"), PortAvailable: func(int) bool { return true }}
}

func TestGatewayExposureRejectsBusyPortAndOffersCertificateBack(t *testing.T) {
	m := New(nil)
	paths := availableGatewayHTTPSPaths(t)
	paths.PortAvailable = func(port int) bool { return port != 80 && port != 443 }
	prompt := &scriptedWorkerSetup{t: t, answers: []string{"standalone", "gateway.example.com", "443", "8443", "auto", "back", "manual", "gateway.example.com"}}
	plan, err := m.chooseGatewayExposureWithHTTPSPaths(context.Background(), prompt, gatewayNginxPaths{}, nil, paths)
	if err != nil || plan == nil || plan.Mode != "manual" || len(prompt.answers) != 0 {
		t.Fatalf("busy setup = %+v, %v", plan, err)
	}
}
