package worker

import (
	"encoding/json"
	"errors"
)

// Goal state is independent of a turn's lifecycle. Only the native active
// status describes a running goal; an idle turn can still have an active goal.
// Project the two display fields, leaving budgets, usage and future additions
// local. Objective text passes through the relay's normal redaction boundary.
type webUIGoal struct {
	Objective string `json:"objective"`
	Status    string `json:"status"`
}

var errWebUIGoalOtherThread = errors.New("thread goal belongs to another thread")

func webUIGoalValue(raw json.RawMessage, thread string) (*webUIGoal, error) {
	var goal map[string]json.RawMessage
	if json.Unmarshal(raw, &goal) != nil || goal == nil {
		return nil, errors.New("invalid thread goal")
	}
	var source, objective, status *string
	if json.Unmarshal(goal["threadId"], &source) != nil || source == nil || *source == "" {
		return nil, errors.New("invalid thread goal identity")
	}
	if *source != thread {
		return nil, errWebUIGoalOtherThread
	}
	if json.Unmarshal(goal["objective"], &objective) != nil || objective == nil || json.Unmarshal(goal["status"], &status) != nil || status == nil {
		return nil, errors.New("invalid thread goal")
	}
	switch *status {
	case "active", "paused", "blocked", "usageLimited", "budgetLimited", "complete":
	default:
		return nil, errors.New("invalid thread goal status")
	}
	return &webUIGoal{Objective: *objective, Status: *status}, nil
}

func webUIGoalPayload(raw json.RawMessage, thread string) (json.RawMessage, error) {
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil || result == nil {
		return nil, errors.New("invalid thread goal response")
	}
	var goal *webUIGoal
	if value := result["goal"]; len(value) > 0 && string(value) != "null" {
		var err error
		goal, err = webUIGoalValue(value, thread)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(struct {
		Goal *webUIGoal `json:"goal"`
	}{Goal: goal})
}

func webUIGoalNotification(rpc webUIRPC, thread string) ([]byte, error) {
	var source string
	if json.Unmarshal(rpc.Params["threadId"], &source) != nil || source != thread {
		return nil, nil
	}
	params := map[string]json.RawMessage{"threadId": rpc.Params["threadId"]}
	if rpc.Method == "thread/goal/updated" {
		goal, err := webUIGoalValue(rpc.Params["goal"], thread)
		if errors.Is(err, errWebUIGoalOtherThread) {
			return nil, nil
		}
		// Unavailable selected-thread data clears any previous indicator. Keep
		// an explicitly foreign nested goal from changing this selection.
		params["goal"], _ = json.Marshal(goal)
	}
	return json.Marshal(webUIRPC{JSONRPC: rpc.JSONRPC, Method: rpc.Method, Params: params})
}
