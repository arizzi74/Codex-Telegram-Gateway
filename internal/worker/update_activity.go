package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
)

// nativeActivity observes one native client's JSON-RPC conversation. Callers
// serialize access with the attachment proxy lock, including observation before
// forwarding a message. An attached but settled connection is not itself work.
type nativeActivity struct {
	uncertain bool
	requests  map[string]*nativeRequest
	approvals map[string]*nativeApproval
	resolved  map[string]struct{}
	turns     map[nativeTurn]struct{}
	completed map[nativeTurn]struct{}
	compact   map[string]struct{}
	threads   map[string]struct{}
	processes map[string]struct{}
	exited    map[string]struct{}
	hooks     map[nativeTurn]struct{}
}

type nativeTurn struct{ thread, turn string }
type nativeRequest struct {
	method, thread, process string
	observedTurn            nativeTurn
}

type nativeApproval struct {
	parent   nativeTurn
	answered bool
}

func (a *nativeActivity) idle() bool { return a.idleError() == nil }

func (a *nativeActivity) idleError() error {
	switch {
	case a.uncertain:
		return errors.New("worker update: native CLI activity could not be verified")
	case len(a.requests) > 0:
		return errors.New("worker update: native CLI requests are still in flight")
	case len(a.approvals) > 0:
		return errors.New("worker update: native CLI approvals or input are pending")
	case len(a.turns) > 0 || len(a.compact) > 0 || len(a.threads) > 0 || len(a.processes) > 0 || len(a.hooks) > 0:
		return errors.New("worker update: a native CLI thread is active or its idle state cannot be verified")
	default:
		return nil
	}
}

func (a *nativeActivity) clientMessage(raw []byte) {
	a.message(raw, true)
}

func (a *nativeActivity) serverMessage(raw []byte) {
	a.message(raw, false)
}

func (a *nativeActivity) message(raw []byte, client bool) {
	object, ok := nativeObject(raw)
	if !ok {
		a.uncertain = true
		return
	}
	if version, present := object["jsonrpc"]; present && nativeString(version) != "2.0" {
		a.uncertain = true
		return
	}
	idRaw, hasID := object["id"]
	id, validID := nativeRequestID(idRaw)
	methodRaw, hasMethod := object["method"]
	_, hasResult := object["result"]
	_, hasError := object["error"]
	if hasID && !validID {
		a.uncertain = true
		return
	}
	if hasMethod {
		method := nativeString(methodRaw)
		if method == "" || hasResult || hasError {
			a.uncertain = true
			return
		}
		params := map[string]json.RawMessage{}
		if raw, present := object["params"]; present {
			params, ok = nativeObject(raw)
			if !ok {
				a.uncertain = true
				return
			}
		}
		if client {
			if hasID {
				a.clientRequest(id, method, params)
			} else if method != "initialized" {
				a.uncertain = true
			}
		} else if hasID {
			if !nativeServerRequest(method) {
				a.uncertain = true
			}
			if a.approvals == nil {
				a.approvals = make(map[string]*nativeApproval)
			}
			if _, duplicate := a.approvals[id]; duplicate {
				a.uncertain = true
			}
			a.approvals[id] = &nativeApproval{parent: nativeTurn{nativeString(params["threadId"]), nativeString(params["turnId"])}}
			delete(a.resolved, id)
		} else {
			a.notification(method, params)
		}
		return
	}
	if !hasID || hasResult == hasError {
		a.uncertain = true
		return
	}
	if hasError {
		if _, valid := nativeObject(object["error"]); !valid {
			a.uncertain = true
			return
		}
	}
	if client {
		if approval, pending := a.approvals[id]; pending {
			if approval.answered {
				a.uncertain = true
			}
			// Writing an answer is not confirmation that Codex consumed it.
			// Keep the approval until the server resolves it or its parent turn
			// completes, including after the terminal disconnects.
			approval.answered = true
		} else {
			if _, wasResolved := a.resolved[id]; !wasResolved {
				a.uncertain = true
			}
			delete(a.resolved, id)
		}
		return
	}
	request, pending := a.requests[id]
	if !pending {
		a.uncertain = true
		return
	}
	delete(a.requests, id)
	if !hasError {
		a.response(request, object["result"])
	}
}

func (a *nativeActivity) clientRequest(id, method string, params map[string]json.RawMessage) {
	if a.requests == nil {
		a.requests = make(map[string]*nativeRequest)
	}
	if _, duplicate := a.requests[id]; duplicate {
		a.uncertain = true
		return
	}
	request := &nativeRequest{method: method, thread: nativeString(params["threadId"]), process: nativeString(params["processHandle"])}
	a.requests[id] = request
	switch method {
	case "turn/start", "turn/steer", "review/start", "thread/compact/start":
		if request.thread == "" {
			a.uncertain = true
		}
		if method == "thread/compact/start" {
			if _, waiting := a.compact[request.thread]; waiting {
				a.uncertain = true
			}
			for otherID, other := range a.requests {
				if otherID != id && other.method == method && other.thread == request.thread {
					a.uncertain = true
				}
			}
		}
	case "process/spawn":
		if request.process == "" {
			a.uncertain = true
		}
		if _, active := a.processes[request.process]; active {
			a.uncertain = true
		}
		for otherID, other := range a.requests {
			if otherID != id && other.method == method && other.process == request.process {
				a.uncertain = true
			}
		}
		delete(a.exited, request.process)
	}
}

func (a *nativeActivity) response(request *nativeRequest, raw json.RawMessage) {
	switch request.method {
	case "turn/start", "turn/steer", "review/start":
		result, ok := nativeObject(raw)
		if !ok {
			a.uncertain = true
			return
		}
		thread := request.thread
		if request.method == "review/start" {
			if reviewThread := nativeString(result["reviewThreadId"]); reviewThread != "" {
				thread = reviewThread
			}
		}
		turnID, status := nativeString(result["turnId"]), ""
		if turnRaw, present := result["turn"]; present {
			turn, valid := nativeObject(turnRaw)
			if !valid {
				a.uncertain = true
				return
			}
			if turnID != "" && turnID != nativeString(turn["id"]) {
				a.uncertain = true
				return
			}
			turnID, status = nativeString(turn["id"]), nativeString(turn["status"])
		}
		key := nativeTurn{thread, turnID}
		if key.thread == "" || key.turn == "" {
			a.uncertain = true
			return
		}
		if nativeTerminalStatus(status) {
			a.finishTurn(key)
		} else if _, finished := a.completed[key]; !finished {
			a.startTurn(key)
		}
	case "thread/compact/start":
		// Compaction may start and finish before its empty RPC reply arrives.
		if request.observedTurn.turn == "" {
			if a.compact == nil {
				a.compact = make(map[string]struct{})
			}
			a.compact[request.thread] = struct{}{}
		}
	case "process/spawn":
		if _, finished := a.exited[request.process]; !finished {
			if a.processes == nil {
				a.processes = make(map[string]struct{})
			}
			a.processes[request.process] = struct{}{}
		}
	case "thread/start", "thread/resume", "thread/fork", "thread/read", "thread/rollback", "thread/revert":
		result, ok := nativeObject(raw)
		if !ok {
			a.uncertain = true
			return
		}
		if threadRaw, present := result["thread"]; present {
			a.threadSnapshot(threadRaw)
		}
	default:
		if !nativeSynchronousRequest(request.method) {
			// In particular shellCommand, queued turns, realtime sessions, and
			// unknown future operations can outlive an acknowledgement.
			a.uncertain = true
		}
	}
}

func (a *nativeActivity) notification(method string, params map[string]json.RawMessage) {
	switch method {
	case "turn/started", "turn/completed":
		turn, ok := nativeObject(params["turn"])
		key := nativeTurn{nativeString(params["threadId"]), nativeString(turn["id"])}
		if !ok || key.thread == "" || key.turn == "" {
			a.uncertain = true
			return
		}
		if method == "turn/completed" {
			a.finishTurn(key)
			return
		}
		for _, request := range a.requests {
			if request.method == "thread/compact/start" && request.thread == key.thread {
				if request.observedTurn.turn != "" && request.observedTurn != key {
					a.uncertain = true
				}
				request.observedTurn = key
			}
		}
		delete(a.compact, key.thread)
		a.startTurn(key)
	case "serverRequest/resolved":
		id, ok := nativeRequestID(params["requestId"])
		if !ok {
			a.uncertain = true
			return
		}
		a.resolveApproval(id)
	case "thread/status/changed":
		a.threadStatus(nativeString(params["threadId"]), params["status"])
	case "thread/started":
		a.threadSnapshot(params["thread"])
	case "hook/started", "hook/completed":
		run, ok := nativeObject(params["run"])
		key := nativeTurn{nativeString(params["threadId"]), nativeString(run["id"])}
		if !ok || key.thread == "" || key.turn == "" {
			a.uncertain = true
			return
		}
		if method == "hook/completed" {
			delete(a.hooks, key)
		} else {
			if a.hooks == nil {
				a.hooks = make(map[nativeTurn]struct{})
			}
			a.hooks[key] = struct{}{}
		}
	case "thread/compacted":
		// Older servers emit this completion without a surrounding turn/start
		// notification. The explicit turn identity also makes a completion
		// received before the compact RPC reply usable as settling evidence.
		key := nativeTurn{nativeString(params["threadId"]), nativeString(params["turnId"])}
		if key.thread == "" || key.turn == "" {
			a.uncertain = true
			return
		}
		delete(a.compact, key.thread)
		for _, request := range a.requests {
			if request.method == "thread/compact/start" && request.thread == key.thread {
				if request.observedTurn.turn != "" && request.observedTurn != key {
					a.uncertain = true
				}
				request.observedTurn = key
			}
		}
	case "process/exited":
		process := nativeString(params["processHandle"])
		if process == "" {
			a.uncertain = true
			return
		}
		delete(a.processes, process)
		if a.exited == nil {
			a.exited = make(map[string]struct{})
		}
		a.exited[process] = struct{}{}
	default:
		if !nativeInformationalNotification(method) {
			a.uncertain = true
		}
	}
}

func (a *nativeActivity) startTurn(key nativeTurn) {
	if a.turns == nil {
		a.turns = make(map[nativeTurn]struct{})
	}
	a.turns[key] = struct{}{}
}

func (a *nativeActivity) finishTurn(key nativeTurn) {
	delete(a.turns, key)
	for id, approval := range a.approvals {
		if approval.parent.thread != "" && approval.parent.turn != "" && approval.parent == key {
			a.resolveApproval(id)
		}
	}
	if a.completed == nil {
		a.completed = make(map[nativeTurn]struct{})
	}
	a.completed[key] = struct{}{}
}

func (a *nativeActivity) resolveApproval(id string) {
	approval, pending := a.approvals[id]
	if !pending {
		return
	}
	if !approval.answered {
		// A server resolution can legitimately overtake the client's answer.
		// Allow that one late reply without treating it as an unknown RPC ID.
		if a.resolved == nil {
			a.resolved = make(map[string]struct{})
		}
		a.resolved[id] = struct{}{}
	}
	delete(a.approvals, id)
}

func (a *nativeActivity) threadSnapshot(raw json.RawMessage) {
	thread, ok := nativeObject(raw)
	id := nativeString(thread["id"])
	if !ok || id == "" {
		a.uncertain = true
		return
	}
	if status, present := thread["status"]; present {
		a.threadStatus(id, status)
	}
	if turnsRaw, present := thread["turns"]; present && !bytes.Equal(bytes.TrimSpace(turnsRaw), []byte("null")) {
		var turns []json.RawMessage
		if json.Unmarshal(turnsRaw, &turns) != nil {
			a.uncertain = true
			return
		}
		for _, raw := range turns {
			turn, valid := nativeObject(raw)
			key := nativeTurn{id, nativeString(turn["id"])}
			status := nativeString(turn["status"])
			if !valid || key.turn == "" || (status != "inProgress" && !nativeTerminalStatus(status)) {
				a.uncertain = true
				return
			}
			if status == "inProgress" {
				if _, finished := a.completed[key]; !finished {
					a.startTurn(key)
				}
			}
		}
	}
}

func (a *nativeActivity) threadStatus(thread string, raw json.RawMessage) {
	status := nativeString(raw)
	if status == "" {
		object, ok := nativeObject(raw)
		if !ok {
			a.uncertain = true
			return
		}
		status = nativeString(object["type"])
	}
	if thread == "" {
		a.uncertain = true
		return
	}
	switch status {
	case "active":
		if a.threads == nil {
			a.threads = make(map[string]struct{})
		}
		a.threads[thread] = struct{}{}
	case "idle", "notLoaded":
		// An idle snapshot never cancels an accepted turn that has not yet
		// published its terminal notification, or pending compaction work.
		delete(a.threads, thread)
	default:
		a.uncertain = true
	}
}

func nativeTerminalStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "interrupted"
}

// The generated Codex RequestId schema is string | int64. Keeping the type in
// the map key prevents numeric 7 and string "7" from acknowledging each other.
func nativeRequestID(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", false
	}
	var value string
	if raw[0] == '"' && json.Unmarshal(raw, &value) == nil {
		return "s:" + value, true
	}
	number, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil {
		return "", false
	}
	return "n:" + strconv.FormatInt(number, 10), true
}

func nativeServerRequest(method string) bool {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/tool/requestUserInput", "tool/requestUserInput", "mcpServer/elicitation/request", "item/permissions/requestApproval", "item/tool/call", "account/chatgptAuthTokens/refresh", "attestation/generate", "currentTime/read", "applyPatchApproval", "execCommandApproval":
		return true
	default:
		return false
	}
}

func nativeString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

// Reject duplicate fields rather than guessing which JSON-RPC identity or
// operation the backend parser will choose. Batches are deliberately uncertain.
func nativeObject(raw []byte) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, false
	}
	object := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		key, ok := token.(string)
		if !ok {
			return nil, false
		}
		if _, duplicate := object[key]; duplicate {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		object[key] = value
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	return object, true
}

func nativeSynchronousRequest(method string) bool {
	switch method {
	case "initialize", "server/diagnostics", "thread/list", "thread/loaded/list", "thread/turns/list", "thread/items/list", "thread/search", "thread/searchOccurrences", "thread/timeline/list",
		"thread/name/set", "thread/metadata/update", "thread/section/move", "thread/settings/update", "thread/memoryMode/set", "thread/goal/get", "thread/goal/clear",
		"thread/archive", "thread/unarchive", "thread/delete", "thread/unsubscribe", "thread/inject_items", "thread/queue/list", "thread/queue/delete", "thread/queue/reorder",
		"thread/backgroundTerminals/list", "thread/backgroundTerminals/clean", "thread/backgroundTerminals/terminate", "turn/interrupt", "turn/settings/update",
		"model/list", "modelProvider/capabilities/read", "collaborationMode/list", "experimentalFeature/list", "permissionProfile/list", "experimentalFeature/enablement/set",
		"config/read", "config/value/write", "config/batchWrite", "configRequirements/read", "config/mcpServer/reload", "skills/list", "skills/config/write", "skills/extraRoots/set", "hooks/list",
		"plugin/list", "plugin/search", "plugin/installed", "plugin/read", "plugin/skill/read", "app/list", "app/read", "app/installed", "mcpServerStatus/list", "mcpServer/resource/read",
		"account/read", "account/rateLimits/read", "account/usage/read", "account/workspaceMessages/read", "account/login/cancel", "account/logout",
		"project/list", "project/read", "project/create", "project/update", "project/move", "project/delete", "threadSection/list", "threadSection/create", "threadSection/update", "threadSection/delete",
		"fs/readFile", "fs/writeFile", "fs/createDirectory", "fs/getMetadata", "fs/readDirectory", "fs/remove", "fs/copy", "fs/watch", "fs/unwatch",
		"command/exec", "command/exec/write", "command/exec/resize", "command/exec/terminate", "process/writeStdin", "process/kill", "process/resizePty",
		"environment/info", "environment/status", "remoteControl/status/read", "fuzzyFileSearch", "fuzzyFileSearch/sessionStop", "currentTime/read":
		return true
	default:
		return false
	}
}

func nativeInformationalNotification(method string) bool {
	switch method {
	case "error", "thread/archived", "thread/deleted", "thread/unarchived", "thread/closed", "thread/reverted", "skills/changed", "thread/name/updated", "thread/goal/updated", "thread/goal/cleared",
		"project/changed", "thread/project/updated", "thread/environment/connected", "thread/environment/disconnected", "thread/settings/updated", "thread/tokenUsage/updated",
		"turn/diff/updated", "turn/plan/updated", "item/started", "item/completed", "item/autoApprovalReview/started", "item/autoApprovalReview/completed", "autoApprovalReview/strictReviewRequired",
		"item/agentMessage/delta", "item/plan/delta", "command/exec/outputDelta", "process/outputDelta", "item/commandExecution/outputDelta", "item/commandExecution/terminalInteraction", "item/fileChange/outputDelta", "item/fileChange/patchUpdated", "item/mcpToolCall/progress",
		"mcpServer/startupStatus/updated", "account/updated", "account/rateLimits/updated", "app/list/updated", "remoteControl/status/changed", "fs/changed", "item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded", "item/reasoning/textDelta",
		"model/rerouted", "model/verification", "modelProvider/authRecoveryStarted", "modelProvider/authRecoveryCompleted", "turn/moderationMetadata", "model/safetyBuffering/updated",
		"warning", "guardianWarning", "deprecationNotice", "configWarning", "windows/worldWritableWarning":
		return true
	default:
		return false
	}
}
