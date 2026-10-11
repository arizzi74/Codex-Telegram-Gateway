package releasemanager

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// This is the version-1 Admin Audio file, rather than a generic configuration
// editor. Keep its accepted model, voice and location names bounded.
type voiceAudioSettings struct {
	Enabled        bool   `json:"enabled"`
	Model          string `json:"model"`
	Threads        int    `json:"threads"`
	ReplyWithAudio bool   `json:"reply_with_audio"`
	TTSVoice       string `json:"tts_voice,omitempty"`
}

type voiceAudioFile struct {
	Version       int                `json:"version"`
	Settings      voiceAudioSettings `json:"settings"`
	ModelLocation string             `json:"model_location"`
}

type voiceAudioState struct {
	dataRoot *os.Root
	audio    *os.Root
	dataInfo os.FileInfo
	dirInfo  os.FileInfo
	fileInfo os.FileInfo
	owner    Ownership
	original []byte
	value    voiceAudioFile
}

func (state *voiceAudioState) close() {
	state.audio.Close()
	state.dataRoot.Close()
}

func validVoiceAudioFile(value voiceAudioFile) bool {
	s := value.Settings
	return value.Version == 1 && (s.Model == "base" || s.Model == "small") && s.Threads >= 1 && s.Threads <= 32 &&
		(s.TTSVoice == "" || s.TTSVoice == "auto" || s.TTSVoice == "italian" || s.TTSVoice == "english") &&
		(value.ModelLocation == "system" || value.ModelLocation == "managed")
}

// openVoiceAudioState never creates a private override for legacy installations.
// Retained directory descriptors confine every privileged read and replacement,
// including if the service account replaces a directory while setup is running.
func openVoiceAudioState(l *Layout, cfg map[string]any) (*voiceAudioState, error) {
	database, _ := cfg["database_path"].(string)
	if database == "" {
		return nil, nil
	}
	if strings.ContainsRune(database, '\x00') || database == ":memory:" || strings.Contains(database, "://") || strings.HasPrefix(database, "file:") {
		return nil, errors.New("voice setup requires a local gateway database path")
	}
	if !filepath.IsAbs(database) {
		database = filepath.Join(filepath.Dir(l.Config), database)
	}
	dataDirectory := filepath.Dir(filepath.Clean(database))
	info, err := os.Lstat(dataDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("gateway audio state requires a real data directory without symlinks")
	}
	root, err := os.OpenRoot(dataDirectory)
	if err != nil {
		return nil, err
	}
	state := &voiceAudioState{dataRoot: root, dataInfo: info}
	accepted := false
	defer func() {
		if !accepted {
			if state.audio != nil {
				state.audio.Close()
			}
			root.Close()
		}
	}()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("gateway data directory changed during voice setup")
	}
	state.dirInfo, err = root.Lstat("audio")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !state.dirInfo.IsDir() || state.dirInfo.Mode()&os.ModeSymlink != 0 || state.dirInfo.Mode().Perm()&0077 != 0 {
		return nil, errors.New("gateway audio state must be a private real directory without symlinks")
	}
	state.audio, err = root.OpenRoot("audio")
	if err != nil {
		return nil, err
	}
	opened, err = state.audio.Stat(".")
	if err != nil || !os.SameFile(state.dirInfo, opened) {
		return nil, errors.New("gateway audio directory changed during voice setup")
	}
	state.fileInfo, err = state.audio.Lstat("settings.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !state.fileInfo.Mode().IsRegular() || state.fileInfo.Mode().Perm()&0077 != 0 || state.fileInfo.Size() > 8192 {
		return nil, errors.New("gateway audio settings must be a private regular file without symlinks")
	}
	// NONBLOCK prevents a regular file swapped for a FIFO from hanging setup.
	file, err := state.audio.OpenFile("settings.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err = file.Stat()
	if err != nil || !os.SameFile(state.fileInfo, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 || opened.Size() > 8192 {
		return nil, errors.New("gateway audio settings changed during voice setup")
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return nil, errors.New("gateway audio settings must not have hard links")
	}
	state.owner = Ownership{UID: int(stat.Uid), GID: int(stat.Gid)}
	state.original, err = io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(state.original) > 8192 {
		return nil, errors.New("could not read bounded gateway audio settings")
	}
	after, err := file.Stat()
	if err != nil || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return nil, errors.New("gateway audio settings changed during voice setup")
	}
	decoder := json.NewDecoder(bytes.NewReader(state.original))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&state.value) != nil || decoder.Decode(&struct{}{}) != io.EOF || !validVoiceAudioFile(state.value) {
		return nil, errors.New("gateway audio settings are not a supported version-1 Audio configuration")
	}
	if err := state.validateDirectories(); err != nil {
		return nil, err
	}
	accepted = true
	return state, nil
}

func (state *voiceAudioState) validateDirectories() error {
	current, err := os.Lstat(state.dataRoot.Name())
	if err != nil || !current.IsDir() || !os.SameFile(state.dataInfo, current) {
		return errors.New("gateway data directory changed during voice setup")
	}
	current, err = state.dataRoot.Lstat("audio")
	if err != nil || !current.IsDir() || !os.SameFile(state.dirInfo, current) {
		return errors.New("gateway audio directory changed during voice setup")
	}
	return nil
}

func resolveVoiceSetupOptionsWithAudio(l *Layout, cfg map[string]any, opts VoiceSetupOptions) (voiceModelSpec, int, error) {
	state, err := openVoiceAudioState(l, cfg)
	if err != nil {
		return voiceModelSpec{}, 0, err
	}
	if state != nil {
		defer state.close()
		if opts.Model == "" {
			opts.Model = state.value.Settings.Model
		}
		if opts.Threads == 0 {
			opts.Threads = state.value.Settings.Threads
		}
	}
	return resolveVoiceSetupOptions(cfg, opts)
}

func (state *voiceAudioState) apply(model string, threads int) (bool, error) {
	if err := state.validateDirectories(); err != nil {
		return false, err
	}
	current, err := state.audio.Lstat("settings.json")
	if err != nil || !os.SameFile(state.fileInfo, current) {
		return false, errors.New("gateway audio settings changed during voice setup; retry with the current settings")
	}
	value := state.value
	value.Settings.Enabled, value.Settings.Model, value.Settings.Threads = true, model, threads
	value.ModelLocation = "system"
	if !validVoiceAudioFile(value) {
		return false, errors.New("voice setup cannot save unsupported audio settings")
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return false, err
	}
	return state.replace(append(data, '\n'))
}

// Like AtomicWrite, replacement keeps ownership on an open temporary file.
// Unlike an absolute-path write, Root.Rename cannot follow a swapped parent
// outside the gateway data directory. The boolean marks the rename commit.
func (state *voiceAudioState) replace(data []byte) (bool, error) {
	suffix, err := randomSuffix()
	if err != nil {
		return false, err
	}
	temporary := ".settings-voice-" + suffix
	file, err := state.audio.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return false, err
	}
	defer state.audio.Remove(temporary)
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return false, err
	}
	if os.Geteuid() == 0 {
		if err := file.Chown(state.owner.UID, state.owner.GID); err != nil {
			return false, err
		}
	}
	if err := file.Chmod(0600); err != nil {
		return false, err
	}
	if err := file.Sync(); err != nil {
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	if err := state.audio.Rename(temporary, "settings.json"); err != nil {
		return false, err
	}
	dir, err := state.audio.Open(".")
	if err != nil {
		return true, err
	}
	defer dir.Close()
	return true, dir.Sync()
}
