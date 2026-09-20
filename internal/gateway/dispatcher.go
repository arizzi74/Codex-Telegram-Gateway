package gateway

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

type CommandStore interface {
	PendingCommandsForWorker(context.Context, uuid.UUID, int) ([]protocol.Command, error)
	MarkDispatched(context.Context, uuid.UUID, uuid.UUID) error
	ExpireCommands(context.Context) error
}

type CommandTransport interface {
	ConnectedWorkers() []uuid.UUID
	SendCommand(context.Context, protocol.Command) error
}

// Dispatcher retries frozen commands until the worker durably acknowledges
// them. Each worker has an ordered stream; independent workers run in parallel.
type Dispatcher struct {
	store     CommandStore
	transport CommandTransport
	log       *slog.Logger
}

func NewDispatcher(store CommandStore, transport CommandTransport, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{store, transport, logger}
}

func (d *Dispatcher) Run(ctx context.Context) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var lastSweep time.Time
	for ctx.Err() == nil {
		if time.Since(lastSweep) >= 5*time.Second {
			sweep, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := d.store.ExpireCommands(sweep); err != nil && ctx.Err() == nil {
				d.log.Warn("command expiry sweep failed", "error", err)
			}
			cancel()
			lastSweep = time.Now()
		}
		d.flush(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
	return nil
}

func (d *Dispatcher) flush(ctx context.Context) {
	var group sync.WaitGroup
	slots := make(chan struct{}, 16)
	for _, workerID := range d.transport.ConnectedWorkers() {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			group.Wait()
			return
		}
		group.Add(1)
		go func() {
			defer group.Done()
			defer func() { <-slots }()
			if err := d.dispatchWorker(ctx, workerID); err != nil && ctx.Err() == nil {
				d.log.Debug("worker dispatch deferred", "worker_id", workerID, "error", err)
			}
		}()
	}
	group.Wait()
}

func (d *Dispatcher) dispatchWorker(ctx context.Context, workerID uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.dispatchWorkerUpdates(ctx, workerID); err != nil {
		return err
	}
	commands, err := d.store.PendingCommandsForWorker(ctx, workerID, 20)
	if err != nil {
		return err
	}
	for _, command := range commands {
		// Record the attempt first. A crash before/after Send is recovered by
		// the bounded retry, using this same ID and immutable target.
		if err := d.store.MarkDispatched(ctx, workerID, uuid.MustParse(command.ID)); err != nil {
			return err
		}
		if err := d.transport.SendCommand(ctx, command); err != nil {
			return err
		}
	}
	return nil
}
