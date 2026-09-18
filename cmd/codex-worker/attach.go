package main

import (
	"errors"
	"strings"

	"github.com/iaia/telegramgw/internal/worker"
)

// attachmentArgs selects the worker-owned endpoint. Latest-session lookup is
// delegated to the native CLI against that live endpoint, so discovery delays
// and worker bookkeeping timestamps cannot select an older conversation.
func attachmentArgs(status worker.Status, args []string) ([]string, error) {
	if len(args) > 1 {
		return nil, errors.New("usage: codex-worker attach [SESSION|--latest]; --latest cannot be combined with a session")
	}
	latest := len(args) == 1 && args[0] == "--latest"
	var thread, runtime string
	if len(args) == 1 && !latest {
		if strings.HasPrefix(args[0], "-") {
			return nil, errors.New("unknown attach option; use codex-worker attach [SESSION|--latest]")
		}
		for _, s := range status.Sessions {
			if s.ID == args[0] || s.ThreadID == args[0] || s.Name == args[0] {
				if thread != "" {
					return nil, errors.New("session name is ambiguous; use the thread ID")
				}
				thread, runtime = s.ThreadID, s.RuntimeID
			}
		}
		if thread == "" {
			return nil, errors.New("session not found")
		}
	}
	var socket string
	for _, r := range status.Runtimes {
		if thread != "" && r.ID != runtime {
			continue
		}
		if r.LocalSocket != "" {
			if socket != "" {
				return nil, errors.New("multiple runtimes; specify a session")
			}
			socket = r.LocalSocket
		}
	}
	if socket == "" {
		return nil, errors.New("runtime has no local attachment socket")
	}
	command := []string{"attach", "--socket", socket}
	if latest {
		command = append(command, "--latest")
	} else if thread != "" {
		command = append(command, thread)
	}
	return command, nil
}
