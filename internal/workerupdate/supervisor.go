package workerupdate

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Runner func(context.Context, ...string) ([]byte, error)

func Run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, args[0], args[1:]...).Output()
}

func ManagerPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, ".local/lib/codex-telegramgw/codex-telegramgw")
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return ""
	}
	return path
}

func Supported() bool {
	if ManagerPath() == "" {
		return false
	}
	command := "systemd-run"
	if runtime.GOOS == "darwin" {
		command = "launchctl"
	} else if runtime.GOOS != "linux" {
		return false
	}
	_, err := exec.LookPath(command)
	return err == nil
}

// Ensure delegates to the user's service manager. The updater must never be
// spawned directly as a child of codex-worker: stopping that service would also
// kill the updater before it could install or start its replacement.
func Ensure(ctx context.Context, system, manager, stateFile, id string, run Runner) error {
	if _, err := requestPath(stateFile, id); err != nil {
		return err
	}
	if !filepath.IsAbs(manager) {
		return errors.New("worker update manager path must be absolute")
	}
	args := []string{manager, "request-update", "worker", "--request-id", id, "--worker-state", stateFile}
	if system == "linux" {
		unit := "codex-worker-request-update-" + id + ".service"
		state, err := run(ctx, "systemctl", "--user", "show", unit, "--property=ActiveState", "--value")
		if err == nil {
			switch strings.TrimSpace(string(state)) {
			case "active", "activating", "reloading":
				return nil
			}
		}
		launch := []string{"systemd-run", "--user", "--collect", "--unit=" + unit, "--property=Type=exec", "--property=Restart=on-failure", "--property=RestartSec=30s", "--property=UMask=0077", "--"}
		_, err = run(ctx, append(launch, args...)...)
		return err
	}
	if system != "darwin" {
		return errors.New("worker update supervision is unsupported")
	}
	label := "com.iaia.codex-worker-request-update-" + id
	domain := "gui/" + strconv.Itoa(os.Getuid())
	if _, err := run(ctx, "launchctl", "print", domain+"/"+label); err == nil {
		return nil
	}
	path := filepath.Join(Directory(stateFile), id+".plist")
	var data bytes.Buffer
	data.WriteString(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>`)
	_ = xml.EscapeText(&data, []byte(label))
	data.WriteString(`</string>`)
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		data.WriteString("<key>" + key + "</key><string>")
		_ = xml.EscapeText(&data, []byte(filepath.Join(Directory(stateFile), id+".log")))
		data.WriteString("</string>")
	}
	data.WriteString(`<key>ProgramArguments</key><array>`)
	for _, arg := range args {
		data.WriteString("<string>")
		_ = xml.EscapeText(&data, []byte(arg))
		data.WriteString("</string>")
	}
	data.WriteString(`</array><key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict><key>ThrottleInterval</key><integer>30</integer></dict></plist>`)
	if err := WritePrivate(path, data.Bytes()); err != nil {
		return err
	}
	_, err := run(ctx, "launchctl", "bootstrap", domain, path)
	return err
}
