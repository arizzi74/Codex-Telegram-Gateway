package admin

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/registry"
)

// The worker route checks fresh authentication, origin, and CSRF before this
// handler. Permanent deletion additionally requires an explicit confirmation.
func (s *Server) purgeWorker(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	var in struct {
		Confirmed bool `json:"confirmed"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirmed {
		bad(w)
		return
	}
	if err := s.store.DeleteRevokedWorker(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, registry.ErrWorkerNotFound):
			http.NotFound(w, r)
		case errors.Is(err, registry.ErrWorkerNotRevoked):
			writeJSON(w, http.StatusConflict, map[string]string{
				"code":    "worker_not_revoked",
				"message": "Revoke this worker before deleting it permanently.",
			})
		default:
			fail(w, err)
		}
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}
