package gateway

import (
	"context"
	"net/http"
	"time"
)

type Readiness interface {
	Ping(context.Context) error
	CheckMigrations(context.Context) error
}

func NewMux(store Readiness, hub *Hub) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /tgapi/v1/workers/connect", hub)
	mux.HandleFunc("GET /tghealthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /tgreadyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := store.Ping(ctx); err != nil {
			http.Error(w, "registry unavailable", 503)
			return
		}
		if err := store.CheckMigrations(ctx); err != nil {
			http.Error(w, "schema not ready", 503)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ready\n"))
	})
	return mux
}
