package releasemanager

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type installerFileInfo struct {
	os.FileInfo
	mode os.FileMode
	uid  uint32
}

func TestGatewayInstallationValidatesOptionalWhisperHelper(t *testing.T) {
	l := &Layout{Bin: "/usr/local/lib/codex-telegramgw", Binary: "/usr/local/lib/codex-telegramgw/codex-gateway", Manager: "/usr/local/lib/codex-telegramgw/codex-telegramgw", Config: "/etc/codex-gateway/gateway.json", State: "/etc/codex-gateway/update.json"}
	helper := filepath.Join(l.Bin, "codex-whisper")
	for _, test := range []struct {
		name  string
		info  os.FileInfo
		valid bool
	}{
		{"missing", nil, true},
		{"root-owned", installerFileInfo{mode: 0755, uid: 0}, true},
		{"service-owned", installerFileInfo{mode: 0755, uid: 1001}, false},
		{"group-writable", installerFileInfo{mode: 0775, uid: 0}, false},
		{"world-writable", installerFileInfo{mode: 0757, uid: 0}, false},
		{"symlink", installerFileInfo{mode: os.ModeSymlink | 0755, uid: 0}, false},
		{"directory", installerFileInfo{mode: os.ModeDir | 0755, uid: 0}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			checked := false
			err := l.validateGatewayInstallation(func(path string) (os.FileInfo, error) {
				if path == helper {
					checked = true
					if test.info == nil {
						return nil, os.ErrNotExist
					}
					return test.info, nil
				}
				return installerFileInfo{mode: 0755, uid: 0}, nil
			})
			if !checked || (err == nil) != test.valid {
				t.Fatalf("helper checked=%v valid=%v error=%v", checked, test.valid, err)
			}
		})
	}
}

func (i installerFileInfo) Mode() os.FileMode { return i.mode }
func (i installerFileInfo) Sys() any          { return &syscall.Stat_t{Uid: i.uid} }

func TestGatewayInstallerRejectsUnsafeExecutableBeforeExecution(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  os.FileMode
		uid   uint32
		valid bool
	}{
		{"root-executable", 0755, 0, true},
		{"private-executable", 0700, 0, true},
		{"service-owned", 0755, 1001, false},
		{"group-writable", 0775, 0, false},
		{"world-writable", 0757, 0, false},
		{"symlink", os.ModeSymlink | 0755, 0, false},
		{"directory", os.ModeDir | 0755, 0, false},
		{"fifo", os.ModeNamedPipe | 0600, 0, false},
		{"device", os.ModeDevice | 0600, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGatewayInstallerPath(installerFileInfo{mode: tc.mode, uid: tc.uid}, true)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
