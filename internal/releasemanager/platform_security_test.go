package releasemanager

import (
	"os"
	"syscall"
	"testing"
)

type installerFileInfo struct {
	os.FileInfo
	mode os.FileMode
	uid  uint32
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
