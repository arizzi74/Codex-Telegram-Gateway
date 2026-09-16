package worker

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/iaia/telegramgw/internal/buildinfo"
	"github.com/iaia/telegramgw/internal/protocol"
)

type Status struct {
	Version   string             `json:"version,omitempty"`
	WorkerID  string             `json:"worker_id"`
	Name      string             `json:"name"`
	PID       int                `json:"pid"`
	UpdatedAt time.Time          `json:"updated_at"`
	Connected bool               `json:"gateway_connected"`
	EventAck  uint64             `json:"event_ack"`
	EventHigh uint64             `json:"event_high"`
	Runtimes  []protocol.Runtime `json:"runtimes"`
	Sessions  []protocol.Session `json:"sessions"`
}

func (a *Agent) writeStatus(connected bool) error {
	ack, high, err := a.store.EventWatermarks()
	if err != nil {
		return err
	}
	sessions, err := a.store.ListSessions("")
	if err != nil {
		return err
	}
	status := Status{Version: buildinfo.Version, WorkerID: a.cfg.WorkerID, Name: a.cfg.Name, PID: os.Getpid(), UpdatedAt: time.Now().UTC(), Connected: connected, EventAck: ack, EventHigh: high, Runtimes: a.manager.Snapshot(), Sessions: sessions}
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	path := a.cfg.StateFile + ".status.json"
	if err = os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func (a *Agent) statusLoop(ctx context.Context, c *Connection) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := a.writeStatus(c.Connected()); err != nil {
			a.log.Warn("write worker diagnostics", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
