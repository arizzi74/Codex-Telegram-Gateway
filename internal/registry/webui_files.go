package registry

import (
	"context"
	"errors"
)

var ErrWebUIFileTarget = errors.New("file session is no longer available")

// CheckWebUIFileTarget keeps an in-flight read bound to its original project,
// session and runtime generation, including after an archive or worker revoke.
func (s *Store) CheckWebUIFileTarget(ctx context.Context, session AdminSession) error {
	var current bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (
	    SELECT 1 FROM sessions s JOIN workers w ON w.worker_id=s.worker_id
	    JOIN runtimes r ON r.runtime_id=s.runtime_id AND r.worker_id=s.worker_id
	    WHERE s.session_id=$1 AND s.worker_id=$2 AND s.runtime_id=$3
	      AND s.codex_thread_id=$4 AND s.cwd=$5 AND s.archived=FALSE
	      AND w.enabled=TRUE AND r.generation=$6 AND r.state='running'
	)`, session.ID, session.WorkerID, session.RuntimeID, session.ThreadID, session.CWD, session.RuntimeGeneration).Scan(&current)
	if err != nil {
		return err
	}
	if !current {
		return ErrWebUIFileTarget
	}
	return nil
}
