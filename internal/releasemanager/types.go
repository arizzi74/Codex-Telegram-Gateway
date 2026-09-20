// Package releasemanager installs and updates checksum-verified gateway releases.
package releasemanager

import (
	"context"
	"io"
	"net/http"
	"time"
)

const DefaultRepo = "arizzi74/Codex-Telegram-Gateway"
const GatewayDataRoot = "/var/lib/codex-gateway"

type Layout struct {
	Component, System, Architecture, Home                              string
	Bin, Binary, Manager, LegacyManager, Command                       string
	Config, Environment, State, Lock, Backups, UnitDir, Unit, DataRoot string
	WorkerServiceAccess                                                string
}

type CommandResult struct {
	Output   []byte
	ExitCode int
}

type Runner func(context.Context, ...string) (CommandResult, error)

type Manager struct {
	Run          Runner
	CodexRun     Runner
	SetupRun     Runner
	Out          io.Writer
	HTTP         *http.Client
	Self         string
	Now          func() time.Time
	ReadyTimeout time.Duration
	PollInterval time.Duration
}

type Release struct {
	Repo, Tag      string
	Assets, Hashes map[string]string
}

type Ownership struct{ UID, GID int }

type BusyError struct{ Reason string }

func (e *BusyError) Error() string { return e.Reason }

// ApplyError records whether a new service could have written durable data.
// After that boundary, rollback must never replace the database or binaries.
type ApplyError struct {
	Err            error
	ServiceStarted bool
}

func (e *ApplyError) Error() string { return e.Err.Error() }
func (e *ApplyError) Unwrap() error { return e.Err }
