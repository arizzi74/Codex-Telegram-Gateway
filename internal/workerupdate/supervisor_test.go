package workerupdate

import (
	"context"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestLinuxUpdaterRunsAsSeparateServiceWithLiteralArguments(t *testing.T) {
	id := uuid.NewString()
	state := filepath.Join(t.TempDir(), "state $(no-shell); space.db")
	manager := "/example home/bin/codex-telegramgw"
	var calls [][]string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{}, args...))
		if args[0] == "systemctl" {
			return []byte("inactive\n"), nil
		}
		return nil, nil
	}
	if err := Ensure(t.Context(), "linux", manager, state, id, run); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1][0] != "systemd-run" {
		t.Fatalf("not independently supervised: %v", calls)
	}
	call := calls[1]
	joined := strings.Join(call, " ")
	for _, fragment := range []string{"--user", "--collect", "--property=Restart=on-failure"} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("missing %s", fragment)
		}
	}
	want := []string{"--", manager, "request-update", "worker", "--request-id", id, "--worker-state", state}
	if !reflect.DeepEqual(call[len(call)-len(want):], want) {
		t.Fatalf("arguments were interpreted: %v", call)
	}
	calls = nil
	run = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte("active\n"), nil
	}
	if err := Ensure(t.Context(), "linux", manager, state, id, run); err != nil || len(calls) != 1 {
		t.Fatalf("replay relaunched active updater: %v %v", calls, err)
	}
}

func TestMacUpdaterUsesLaunchdArgumentArray(t *testing.T) {
	id := uuid.NewString()
	state := filepath.Join(t.TempDir(), "state & <value>.db")
	var calls [][]string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{}, args...))
		if args[1] == "print" {
			return nil, errors.New("not loaded")
		}
		return nil, nil
	}
	if err := Ensure(t.Context(), "darwin", "/example/bin/manager", state, id, run); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1][1] != "bootstrap" {
		t.Fatalf("not launchd supervised: %v", calls)
	}
	data, err := os.ReadFile(calls[1][3])
	if err != nil {
		t.Fatal(err)
	}
	decoder := xml.NewDecoder(strings.NewReader(string(data)))
	var stringsFound []string
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local == "string" {
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				t.Fatal(err)
			}
			stringsFound = append(stringsFound, value)
		}
	}
	if stringsFound[len(stringsFound)-1] != state {
		t.Fatalf("plist lost literal path: %s", data)
	}
}
