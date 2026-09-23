package config

import "testing"

func TestNormalizeGatewayURLMigratesOnlyLegacyWorkerEndpoint(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"wss://gateway.example.com/api/v1/workers/connect", "wss://gateway.example.com/tgw/api/v1/workers/connect"},
		{"wss://gateway.example.com:8443/api/v1/workers/connect/", "wss://gateway.example.com:8443/tgw/api/v1/workers/connect"},
		{"wss://[::1]:8443/api/v1/workers/connect", "wss://[::1]:8443/tgw/api/v1/workers/connect"},
		{"wss://gateway.example.com/tgapi/v1/workers/connect", "wss://gateway.example.com/tgw/api/v1/workers/connect"},
		{"wss://gateway.example.com:8443/tgapi/v1/workers/connect/", "wss://gateway.example.com:8443/tgw/api/v1/workers/connect"},
		{"wss://gateway.example.com/tgw/api/v1/workers/connect", ""},
		{"wss://gateway.example.com/custom/api/v1/workers/connect", ""},
		{"wss://gateway.example.com/api/v1/workers/connect/other", ""},
		{"wss://gateway.example.com/%61pi/v1/workers/connect", ""},
		{"wss://gateway.example.com/tgapi/v1/workers/connect?", ""},
		{"wss://gateway.example.com/%74gapi/v1/workers/connect", ""},
		{"wss://gateway.example.com/api/v1/workers/connect?secret=value", ""},
		{"wss://gateway.example.com/api/v1/workers/connect?", ""},
		{"wss://gateway.example.com/api/v1/workers/connect#", ""},
		{"wss://user:password@gateway.example.com/api/v1/workers/connect", ""},
		{"https://gateway.example.com/api/v1/workers/connect", ""},
		{"ws://gateway.example.com/api/v1/workers/connect", ""},
		{"wss:///api/v1/workers/connect", ""},
		{"%invalid", ""},
	} {
		t.Run(test.input, func(t *testing.T) {
			want := test.want
			if want == "" {
				want = test.input
			}
			if got := NormalizeGatewayURL(test.input); got != want {
				t.Fatalf("NormalizeGatewayURL(%q) = %q; want %q", test.input, got, want)
			}
		})
	}
}
