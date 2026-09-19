package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTelegramScopedMenuPayload(t *testing.T) {
	for _, scope := range []BotCommandScope{
		{Type: "chat", ChatID: 123},
		{Type: "chat_member", ChatID: -100123, UserID: 123},
	} {
		t.Run(scope.Type, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var payload struct {
					Scope    BotCommandScope `json:"scope"`
					Commands []BotCommand    `json:"commands"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if payload.Scope != scope {
					t.Errorf("scope = %#v, want %#v", payload.Scope, scope)
				}
				switch calls {
				case 1:
					if r.URL.Path != "/botfake-token/setMyCommands" || len(payload.Commands) != 1 || payload.Commands[0].Command != "_my_project" {
						t.Errorf("set payload/path = %#v %s", payload, r.URL.Path)
					}
				case 2:
					if r.URL.Path != "/botfake-token/deleteMyCommands" || len(payload.Commands) != 0 {
						t.Errorf("delete payload/path = %#v %s", payload, r.URL.Path)
					}
				default:
					t.Errorf("unexpected call %d", calls)
				}
				w.Write([]byte(`{"ok":true,"result":true}`))
			}))
			defer server.Close()
			client := NewTelegramClient("fake-token")
			client.endpoint, client.http = server.URL, server.Client()
			if err := client.SetScopedCommands(context.Background(), scope, []BotCommand{{Command: "_my_project", Description: "My project"}}); err != nil {
				t.Fatal(err)
			}
			if err := client.DeleteScopedCommands(context.Background(), scope); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("calls = %d", calls)
			}
		})
	}
}
