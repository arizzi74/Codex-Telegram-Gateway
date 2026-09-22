package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"time"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
	"github.com/iaia/telegramgw/internal/workerupdate"
)

func (c *Connection) receiveWorkerUpdate(request protocol.WorkerUpdateRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if request.WorkerID != c.cfg.WorkerID {
		return errors.New("worker update targets another worker")
	}
	if err := workerupdate.Create(c.cfg.StateFile, request); err != nil {
		return err
	}
	select {
	case c.updateWake <- struct{}{}:
	default:
	}
	return nil
}

func (c *Connection) workerUpdateLoop(ctx context.Context) {
	poll := time.NewTicker(5 * time.Second)
	defer poll.Stop()
	for {
		if err := c.reconcileWorkerUpdates(ctx); err != nil && ctx.Err() == nil {
			// Files and gateway delivery remain durable; do not drop the worker
			// connection because a local supervisor is temporarily unavailable.
			c.log.Warn("worker update queue reconciliation failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		case <-c.updateWake:
		}
	}
}

func (c *Connection) reconcileWorkerUpdates(ctx context.Context) error {
	requests, err := workerupdate.List(c.cfg.StateFile)
	if err != nil {
		return err
	}
	for _, request := range requests {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if request.WorkerID != c.cfg.WorkerID {
			continue
		}
		result, err := workerupdate.Result(c.cfg.StateFile, request.RequestID)
		if errors.Is(err, os.ErrNotExist) {
			if c.updateManager == "" {
				result = protocol.WorkerUpdateResult{RequestID: request.RequestID, State: "failed", ErrorCode: "update_unavailable"}
				if err = workerupdate.Complete(c.cfg.StateFile, result); err != nil {
					return err
				}
			} else {
				if err := workerupdate.Ensure(ctx, runtime.GOOS, c.updateManager, c.cfg.StateFile, request.RequestID, c.updateRun); err != nil {
					// Supervisor failures may be temporary during login or restart.
					// Leave this request queued and try again without changing timers.
					return err
				}
				continue
			}
		} else if err != nil {
			return err
		}
		if err := c.store.recordWorkerUpdateResult(result); err != nil {
			return err
		}
	}
	return nil
}

// Marking the request and creating its event share one SQLite transaction. A
// crash after writing the external result, before or after gateway ACK, cannot
// lose the outcome or generate duplicate completion messages.
func (s *Store) recordWorkerUpdateResult(result protocol.WorkerUpdateResult) error {
	if err := result.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *workerdb.Tx) error {
		key := []byte("worker_update_result/" + result.RequestID)
		meta := tx.Bucket(bucketMeta)
		if meta.Get(key) != nil {
			return nil
		}
		if _, err := s.appendEvent(tx, protocol.Event{Kind: "worker_update_result", Data: data}); err != nil {
			return err
		}
		return meta.Put(key, data)
	})
}
