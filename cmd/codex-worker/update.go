package main

import (
	"context"
	"encoding/json"
	"io"

	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/worker"
)

func requestUpdate(ctx context.Context, cfg config.WorkerConfig, action, token string, stdout io.Writer) error {
	lease, err := worker.RequestUpdate(ctx, cfg, action, token)
	if err != nil {
		if action == "prepare" {
			// The release manager captures stdout independently of stderr. Keep
			// the rejection machine-readable without changing the failure exit
			// status or the usual human-readable error on stderr.
			message := err.Error()
			if len(message) > 1024 {
				message = "worker update: preparation failed; inspect the worker logs"
			}
			_ = json.NewEncoder(stdout).Encode(struct {
				Error string `json:"error"`
			}{Error: message})
		}
		return err
	}
	if action == "abort" {
		return json.NewEncoder(stdout).Encode(map[string]bool{"aborted": true})
	}
	return json.NewEncoder(stdout).Encode(lease)
}
