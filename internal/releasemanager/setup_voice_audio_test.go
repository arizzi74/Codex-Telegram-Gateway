package releasemanager

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func audioSetupFile(t *testing.T, directory string) string {
	t.Helper()
	path := filepath.Join(directory, "audio", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	value := voiceAudioFile{Version: 1, ModelLocation: "managed", Settings: voiceAudioSettings{Enabled: false, Model: "small", Threads: 5, ReplyWithAudio: true, TTSVoice: "italian"}}
	if err := WriteJSON(path, value); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVoiceSetupOptionsPreferAdminAudioAndExplicitFlags(t *testing.T) {
	directory := t.TempDir()
	l := &Layout{Config: filepath.Join(directory, "gateway.json")}
	cfg := map[string]any{"database_path": "gateway.db", "voice": map[string]any{"model_path": voiceModelPath, "threads": float64(2)}}
	audioSetupFile(t, directory)
	for _, test := range []struct {
		name    string
		opts    VoiceSetupOptions
		model   string
		threads int
	}{
		{"omitted", VoiceSetupOptions{}, "small", 5},
		{"model override", VoiceSetupOptions{Model: "base"}, "base", 5},
		{"threads override", VoiceSetupOptions{Threads: 3}, "small", 3},
		{"both overrides", VoiceSetupOptions{Model: "base", Threads: 1}, "base", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			model, threads, err := resolveVoiceSetupOptionsWithAudio(l, cfg, test.opts)
			if err != nil || model.Name != test.model || threads != test.threads {
				t.Fatalf("wrong Admin/CLI precedence: %s %d %v", model.Name, threads, err)
			}
		})
	}
}

func TestVoiceAudioReplacementPreservesSpeechChoicesAndOwnership(t *testing.T) {
	directory := t.TempDir()
	path := audioSetupFile(t, directory)
	before, _ := os.Stat(path)
	l := &Layout{Config: filepath.Join(directory, "gateway.json")}
	state, err := openVoiceAudioState(l, map[string]any{"database_path": filepath.Join(directory, "gateway.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	committed, err := state.apply("base", 3)
	if err != nil || !committed {
		t.Fatalf("audio settings not committed: %v", err)
	}
	var saved voiceAudioFile
	if err := json.Unmarshal([]byte(updateRead(t, path)), &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Settings.Enabled || saved.Settings.Model != "base" || saved.Settings.Threads != 3 || saved.ModelLocation != "system" || !saved.Settings.ReplyWithAudio || saved.Settings.TTSVoice != "italian" {
		t.Fatalf("speech choices or selected Whisper configuration lost: %+v", saved)
	}
	after, _ := os.Stat(path)
	oldOwner, newOwner := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if after.Mode().Perm() != 0600 || oldOwner.Uid != newOwner.Uid || oldOwner.Gid != newOwner.Gid {
		t.Fatal("private audio file lost its gateway ownership or 0600 permissions")
	}
}

func TestVoiceAudioLegacySetupDoesNotCreateOverrides(t *testing.T) {
	directory := t.TempDir()
	l := &Layout{Config: filepath.Join(directory, "gateway.json")}
	cfg := map[string]any{"database_path": filepath.Join(directory, "gateway.db"), "voice": map[string]any{"model_path": voiceSmallModelPath, "threads": float64(3)}}
	model, threads, err := resolveVoiceSetupOptionsWithAudio(l, cfg, VoiceSetupOptions{})
	if err != nil || model.Name != "small" || threads != 3 {
		t.Fatalf("legacy options changed: %s %d %v", model.Name, threads, err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "audio")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reading legacy options created Admin Audio overrides")
	}
}

func TestVoiceAudioStateRejectsUnsupportedOrUntrustedFiles(t *testing.T) {
	for _, mutation := range []string{"directory link", "file link", "hard link", "data link", "public directory", "public file", "oversize", "new schema", "unknown field", "model path", "voice", "threads", "model location"} {
		t.Run(mutation, func(t *testing.T) {
			directory := t.TempDir()
			path := audioSetupFile(t, directory)
			cfg := map[string]any{"database_path": filepath.Join(directory, "gateway.db")}
			outside := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(outside, []byte("outside state must remain unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			data := updateRead(t, path)
			switch mutation {
			case "directory link":
				if err := os.RemoveAll(filepath.Dir(path)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(outside), filepath.Dir(path)); err != nil {
					t.Fatal(err)
				}
			case "file link", "hard link":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				link := os.Symlink
				if mutation == "hard link" {
					link = os.Link
				}
				if err := link(outside, path); err != nil {
					t.Fatal(err)
				}
			case "data link":
				link := filepath.Join(t.TempDir(), "data")
				if err := os.Symlink(directory, link); err != nil {
					t.Fatal(err)
				}
				cfg["database_path"] = filepath.Join(link, "gateway.db")
			case "public file":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "public directory":
				if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				data = strings.Repeat(" ", 8193)
			case "new schema":
				data = strings.Replace(data, `"version": 1`, `"version": 2`, 1)
			case "unknown field":
				data = strings.Replace(data, `"version": 1`, `"version": 1, "password": "never read arbitrary fields"`, 1)
			case "model path":
				data = strings.Replace(data, `"model": "small"`, `"model": "/tmp/arbitrary-model"`, 1)
			case "voice":
				data = strings.Replace(data, `"tts_voice": "italian"`, `"tts_voice": "unsupported"`, 1)
			case "threads":
				data = strings.Replace(data, `"threads": 5`, `"threads": 33`, 1)
			case "model location":
				data = strings.Replace(data, `"model_location": "managed"`, `"model_location": "/tmp/model"`, 1)
			}
			if data != updateRead(t, path) && !strings.HasSuffix(mutation, "link") {
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			state, err := openVoiceAudioState(&Layout{Config: filepath.Join(directory, "gateway.json")}, cfg)
			if state != nil {
				state.close()
			}
			if err == nil {
				t.Fatalf("accepted %s", mutation)
			}
			if updateRead(t, outside) != "outside state must remain unchanged" {
				t.Fatal("untrusted state escaped the gateway directory")
			}
		})
	}
}

func TestVoiceAudioDirectorySwapCannotRedirectPrivilegedReplacement(t *testing.T) {
	directory := t.TempDir()
	path := audioSetupFile(t, directory)
	state, err := openVoiceAudioState(&Layout{Config: filepath.Join(directory, "gateway.json")}, map[string]any{"database_path": filepath.Join(directory, "gateway.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	outside := t.TempDir()
	outsidePath := filepath.Join(outside, "settings.json")
	if err := os.WriteFile(outsidePath, []byte("do not replace"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Dir(path), filepath.Join(directory, "audio-original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if committed, err := state.apply("base", 3); err == nil || committed {
		t.Fatal("a replaced audio directory must abort setup")
	}
	// Even recovery writes remain in the originally opened directory.
	if _, err := state.replace(state.original); err != nil {
		t.Fatal(err)
	}
	if updateRead(t, outsidePath) != "do not replace" {
		t.Fatal("recovery followed an attacker-controlled directory symlink")
	}
}

func TestVoiceApplySynchronizesAndRollsBackExistingAdminAudio(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("gateway readiness uses Linux process identity")
	}
	for _, failure := range []string{"", "restart", "notices", "readiness"} {
		t.Run("failure="+failure, func(t *testing.T) {
			m, l, helper, model, notices := voiceApplyFixture(t)
			path := audioSetupFile(t, l.DataRoot)
			original := updateRead(t, path)
			configBefore, helperBefore := updateRead(t, l.Config), updateRead(t, filepath.Join(l.Bin, "codex-whisper"))
			l.Binary, _ = os.Executable()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ready\n")) }))
			defer server.Close()
			cfg, _ := ReadJSON(l.Config)
			cfg["listen"] = strings.TrimPrefix(server.URL, "http://")
			if err := WriteJSON(l.Config, cfg); err != nil {
				t.Fatal(err)
			}
			configBefore = updateRead(t, l.Config)
			if failure == "notices" {
				notices = filepath.Join(t.TempDir(), "missing")
			}
			restarts := 0
			m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
				if args[0] != "systemctl" {
					t.Fatalf("unexpected command: %v", args)
				}
				if args[1] == "restart" {
					restarts++
					if restarts == 1 {
						var saved voiceAudioFile
						if err := json.Unmarshal([]byte(updateRead(t, path)), &saved); err != nil || !saved.Settings.Enabled || saved.Settings.Model != "small" || saved.Settings.Threads != 3 || saved.ModelLocation != "system" || saved.Settings.TTSVoice != "italian" || !saved.Settings.ReplyWithAudio {
							t.Fatalf("restart preceded synchronized Audio settings: %+v %v", saved, err)
						}
						if failure == "restart" {
							return CommandResult{}, errors.New("restart failed")
						}
					} else if updateRead(t, path) != original {
						t.Fatal("recovery restart preceded original Audio settings")
					}
				}
				if failure == "readiness" {
					return CommandResult{}, nil
				}
				return CommandResult{Output: []byte("ActiveState=active\nMainPID=" + strconv.Itoa(os.Getpid()) + "\n")}, nil
			}
			if failure == "" {
				m.ReadyTimeout = time.Second
			}
			err := m.applyVoiceModel(t.Context(), l, helper, model, notices, "small", 3)
			if (failure == "") != (err == nil) {
				t.Fatalf("unexpected apply result: %v", err)
			}
			if failure != "" && (updateRead(t, path) != original || updateRead(t, l.Config) != configBefore || updateRead(t, filepath.Join(l.Bin, "codex-whisper")) != helperBefore) {
				t.Fatal("failed CLI setup did not restore Audio settings, gateway configuration and helper")
			}
			backups, err := filepath.Glob(filepath.Join(l.Backups, "voice-*", "audio-settings.json"))
			if err != nil || len(backups) != 1 || updateRead(t, backups[0]) != original {
				t.Fatal("voice backup did not include the original private Audio state")
			}
		})
	}
}
