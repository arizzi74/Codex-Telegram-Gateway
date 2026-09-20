package gateway

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

var ErrWorkerUpdateUnsupported = errors.New("worker does not support queued updates")

type workerUpdateStore interface {
	PendingWorkerUpdates(context.Context, uuid.UUID) ([]protocol.WorkerUpdateRequest, error)
	MarkWorkerUpdateDispatched(context.Context, uuid.UUID, uuid.UUID) error
	FailWorkerUpdate(context.Context, uuid.UUID, uuid.UUID, string) error
}

type workerUpdateTransport interface {
	SendWorkerUpdate(context.Context, protocol.WorkerUpdateRequest) error
}

func (h *Hub) SendWorkerUpdate(ctx context.Context, request protocol.WorkerUpdateRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	h.mu.RLock()
	p := h.peers[request.WorkerID]
	h.mu.RUnlock()
	if p == nil {
		return errors.New("worker is not connected")
	}
	if err := h.store.CheckConnection(ctx, p.workerID, p.connectionID); err != nil {
		return err
	}
	if !p.supportsWorkerUpdate {
		return ErrWorkerUpdateUnsupported
	}
	return p.send(ctx, "worker_update_request", request)
}

func (d *Dispatcher) dispatchWorkerUpdates(ctx context.Context, workerID uuid.UUID) error {
	store, ok := d.store.(workerUpdateStore)
	if !ok {
		return nil
	}
	transport, ok := d.transport.(workerUpdateTransport)
	if !ok {
		return nil
	}
	requests, err := store.PendingWorkerUpdates(ctx, workerID)
	if err != nil {
		return err
	}
	for _, request := range requests {
		if err := request.Validate(); err != nil || request.WorkerID != workerID.String() {
			return errors.New("invalid queued worker update target")
		}
		id := uuid.MustParse(request.RequestID)
		if err := store.MarkWorkerUpdateDispatched(ctx, workerID, id); err != nil {
			return err
		}
		if err := transport.SendWorkerUpdate(ctx, request); err != nil {
			if !errors.Is(err, ErrWorkerUpdateUnsupported) {
				return err
			}
			if err := store.FailWorkerUpdate(ctx, workerID, id, "unsupported_worker"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Sender) renderWorkerUpdates(response registry.AcceptResult) string {
	if len(response.WorkerUpdates) == 0 {
		return "No enabled workers are enrolled."
	}
	lines := []string{"Worker updates"}
	for _, worker := range response.WorkerUpdates {
		name := s.sessionListField(worker.Name, 120, 240)
		if name == "" {
			name = worker.WorkerID
		}
		status := "Update request failed. Check the worker's update service logs and try /tgupdateworkers again."
		switch worker.State {
		case "queued":
			status = "Queued"
		case "already_queued":
			status = "Already queued"
		case "completed":
			status = "Updated and restarted · " + worker.Version
		case "up_to_date":
			status = "Already up to date · " + worker.Version + " · No restart"
		case "failed":
			switch worker.ErrorCode {
			case "unsupported_worker", "update_unavailable":
				status = "This worker needs a local update first: codex-telegramgw update worker"
			}
		}
		lines = append(lines, "\n"+name+"\n"+status)
	}
	if response.View == "worker_updates" {
		lines = append(lines, "\nEach worker will check GitHub and update at its next idle point, after all its turns and pending work finish. Offline workers receive the request when they reconnect. Workers already on the latest version are not restarted.")
	}
	return strings.Join(lines, "\n")
}
