package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

func verifyRuntimeUpdateIdle(ctx context.Context, client *codexadapter.Client, queues []string, candidates map[string]string) (map[string]string, error) {
	if len(queues) > discoveryLimit || len(candidates) > discoveryLimit {
		return nil, errors.New("worker update: too many native queues or failed subagents to verify")
	}
	proofs := make(map[string]string)
	verified := make(map[string]bool)
	visiting := make(map[string]bool)
	queueIdle := func(threadID string) error {
		if err := client.VerifyThreadQueueIdle(ctx, threadID); err != nil {
			return errors.New("worker update: native CLI queued work is pending or cannot be verified")
		}
		return nil
	}
	// Queue consumption can start a turn, so inspect queues before thread state.
	for _, threadID := range queues {
		if err := queueIdle(threadID); err != nil {
			return nil, err
		}
	}
	var verify func(string) error
	verify = func(threadID string) error {
		if visiting[threadID] {
			return errors.New("worker update: failed subagent ancestry cannot be verified")
		}
		if verified[threadID] {
			return nil
		}
		if threadID == "" || len(verified)+len(visiting) >= discoveryLimit {
			return errors.New("worker update: too many native threads to verify")
		}
		visiting[threadID] = true
		defer delete(visiting, threadID)
		thread, err := client.ReadThreadState(ctx, threadID)
		if err != nil {
			return fmt.Errorf("worker update: cannot verify native thread: %w", err)
		}
		_, candidate := candidates[threadID]
		if thread.ActiveTurnID == "" && (thread.Status == "idle" || thread.Status == "notLoaded" || thread.Status == "not_loaded") && !candidate {
			verified[threadID] = true
			return nil
		}
		if !thread.TerminalFailedSubAgent() {
			return errors.New("worker update: a native CLI thread is active or its idle state cannot be verified")
		}
		turnID := thread.LatestTurn.ID
		if expected := candidates[threadID]; expected != "" && expected != turnID {
			return errors.New("worker update: failed subagent turn does not match observed native activity")
		}
		// A terminal failed turn alone does not establish that its queue is empty
		// or its parent is idle. Verify both, even outside workspace discovery.
		if err := queueIdle(threadID); err != nil {
			return err
		}
		if err := queueIdle(thread.ParentThreadID); err != nil {
			return err
		}
		if err := verify(thread.ParentThreadID); err != nil {
			return err
		}
		fresh, err := client.ReadThreadState(ctx, threadID)
		if err != nil || !fresh.TerminalFailedSubAgent() || fresh.LatestTurn.ID != turnID || fresh.ParentThreadID != thread.ParentThreadID {
			return errors.New("worker update: failed subagent changed during idle verification")
		}
		proofs[threadID] = turnID
		verified[threadID] = true
		return nil
	}
	for _, threadID := range queues {
		if err := verify(threadID); err != nil {
			return nil, err
		}
	}
	// Detached native connections may reference threads absent from the loaded
	// inventory. Their markers still require independent, exact proof.
	for threadID := range candidates {
		if err := verify(threadID); err != nil {
			return nil, err
		}
	}
	cursor := ""
	seen := make(map[string]bool)
	count := 0
	for {
		page, err := client.LoadedThreads(ctx, cursor, discoveryPageSize)
		if err != nil {
			return nil, fmt.Errorf("worker update: cannot verify loaded threads: %w", err)
		}
		for _, entry := range page.Threads {
			count++
			if count > discoveryLimit {
				return nil, errors.New("worker update: too many loaded threads to verify")
			}
			// Every loaded descendant, not just the failed child, must be idle.
			if err := verify(entry.ID); err != nil {
				return nil, err
			}
		}
		if page.NextCursor == "" {
			break
		}
		if seen[page.NextCursor] {
			return nil, errors.New("worker update: repeated loaded-thread cursor")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
	if len(proofs) > 0 {
		// Recovery can retire a whole runtime. Idle descendants can still own
		// queued prompts or background terminals, so verify every thread seen
		// above, including ancestors absent from the loaded inventory. Re-read
		// state after queue checks, since consuming a prompt can start a turn.
		for threadID := range verified {
			if err := queueIdle(threadID); err != nil {
				return nil, err
			}
			if err := client.VerifyThreadBackgroundTerminalsIdle(ctx, threadID); err != nil {
				return nil, errors.New("worker update: background terminals are active or cannot be verified")
			}
			fresh, err := client.ReadThreadState(ctx, threadID)
			if err != nil {
				return nil, errors.New("worker update: native thread changed during failed subagent recovery")
			}
			if turnID := proofs[threadID]; turnID != "" {
				if !fresh.TerminalFailedSubAgent() || fresh.LatestTurn.ID != turnID {
					return nil, errors.New("worker update: failed subagent changed during idle verification")
				}
			} else if fresh.ActiveTurnID != "" || (fresh.Status != "idle" && fresh.Status != "notLoaded" && fresh.Status != "not_loaded") {
				return nil, errors.New("worker update: a native CLI thread is active or its idle state cannot be verified")
			}
		}
	}
	return proofs, nil
}
