package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Pending questions are independent from the bounded transcript window. Only
// worker approval IDs cross this snapshot boundary; native transport IDs stay
// on the observer connection that owns the request and its eventual answer.
type webUIQuestion struct {
	ApprovalID string                     `json:"approvalId"`
	Async      bool                       `json:"async"`
	Method     string                     `json:"method"`
	Params     map[string]json.RawMessage `json:"params"`
}

type webUIQuestionAnswer struct {
	ApprovalID string          `json:"approvalId"`
	Result     json.RawMessage `json:"result"`
}

func webUIQuestionParams(method string, params map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if method == "gateway/questions" {
		if len(params) != 0 {
			return nil, validationError("Question snapshots are bound to the selected session.")
		}
		return params, nil
	}
	var id string
	var result map[string]json.RawMessage
	if len(params) != 2 || json.Unmarshal(params["approvalId"], &id) != nil || id == "" || len(id) > 256 || len(params["result"]) > 256<<10 || json.Unmarshal(params["result"], &result) != nil || result == nil {
		return nil, validationError("A current approval ID and bounded answer are required.")
	}
	return params, nil
}

func (r *webUIRelay) questions(rpc webUIRPC) error {
	ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
	defer cancel()
	var answer *webUIQuestionAnswer
	if rpc.Method == "gateway/answer" {
		answer = &webUIQuestionAnswer{Result: rpc.Params["result"]}
		_ = json.Unmarshal(rpc.Params["approvalId"], &answer.ApprovalID)
	}
	var questions []webUIQuestion
	var err error
	if lookup := r.pool.c.questionsWebUI; lookup != nil {
		questions, err = lookup(ctx, r.runtime, r.session, answer)
	} else {
		err = validationError("Update this worker to restore pending questions in the Web UI.")
	}
	r.mu.Lock()
	delete(r.pending, webUIID(rpc.ID))
	r.mu.Unlock()
	reply := webUIRPC{ID: rpc.ID}
	if err != nil {
		message := "The question state could not be confirmed. Reconnect before retrying; answers are never replayed."
		var invalid *CodexCommandValidationError
		if errors.As(err, &invalid) {
			message = safeErrorMessage(r.pool.c.store.redactor.Load(), invalid.Message, message)
		}
		reply.Error, _ = json.Marshal(map[string]any{"code": -32000, "message": message})
	} else if answer != nil {
		reply.Result = json.RawMessage(`{"state":"completed"}`)
	} else {
		if questions == nil {
			questions = []webUIQuestion{}
		}
		reply.Result, _ = json.Marshal(map[string]any{"questions": questions})
	}
	data, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	data, err = redactWebUIJSON(r.pool.c.store.redactor.Load(), data)
	if err != nil {
		return err
	}
	return r.pool.send(r.ctx, protocol.WebUIFrame{ID: r.id, Action: "output", Data: data})
}

func (a *Agent) webUIQuestions(ctx context.Context, runtime protocol.Runtime, session protocol.Session, answer *webUIQuestionAnswer) ([]webUIQuestion, error) {
	reply, err := a.dispatchWebUIActor(webUIActorCommand{ctx: ctx, runtime: runtime, session: session, questions: true, answer: answer})
	return reply.questions, err
}

func (s *sessionActor) webUIQuestions(request webUIActorCommand) ([]webUIQuestion, error) {
	if err := request.ctx.Err(); err != nil {
		return nil, err
	}
	if request.runtime.ID != s.runtime.ID || request.runtime.Generation != s.runtime.Generation || request.session.ID != s.session.ID || request.session.ThreadID != s.session.ThreadID || request.session.WorkerID != s.agent.cfg.WorkerID || request.session.RuntimeID != s.runtime.ID || s.session.Archived || s.session.Deleted {
		return nil, validationError("The question's session or runtime changed. Reconnect to refresh it.")
	}
	client, runtime, ok := s.agent.manager.Client(s.runtime.ID)
	if !ok || runtime.Generation != request.runtime.Generation || runtime.State != "running" {
		return nil, validationError("The question's runtime is unavailable.")
	}
	if _, err := auth.CanonicalWorkspace(s.session.CWD, s.agent.cfg.AllowedWorkspaceRoots); err != nil {
		return nil, validationError("The question's session workspace is no longer allowed.")
	}
	if request.answer != nil {
		return nil, s.webUIAnswerQuestion(request.ctx, client, *request.answer)
	}
	questions := make([]webUIQuestion, 0, len(s.pending)+len(s.asyncQuestions))
	for _, pending := range s.pending {
		if pending.approval.ThreadID != s.session.ThreadID || !client.RequestPending(pending.request.RequestID) || (pending.approval.TurnID != "" && pending.approval.TurnID != s.session.ActiveTurnID) {
			continue
		}
		if question, ok := webUIPendingQuestion(pending); ok {
			questions = append(questions, question)
		}
	}
	for _, approval := range s.asyncQuestions {
		if approval.ThreadID != s.session.ThreadID || approval.State != "pending" || len(approval.Questions) == 0 {
			continue
		}
		params := map[string]json.RawMessage{}
		params["threadId"], _ = json.Marshal(s.session.ThreadID)
		params["turnId"], _ = json.Marshal(approval.TurnID)
		params["itemId"], _ = json.Marshal(approval.ItemID)
		fields := make([]map[string]any, 0, len(approval.Questions))
		for _, field := range approval.Questions {
			options := make([]map[string]string, 0, len(field.Options))
			for _, label := range field.Options {
				options = append(options, map[string]string{"label": label})
			}
			fields = append(fields, map[string]any{"id": field.ID, "header": field.Header, "question": field.Prompt, "isSecret": field.Secret, "options": options})
		}
		params["questions"], _ = json.Marshal(fields)
		questions = append(questions, webUIQuestion{ApprovalID: approval.ID, Async: true, Method: "item/tool/requestUserInput", Params: params})
	}
	if len(questions) > 64 {
		return nil, validationError("More than 64 requests are pending in this session. Answer existing questions before refreshing the list.")
	}
	encoded, err := json.Marshal(questions)
	if err != nil || len(encoded) > 1<<20 {
		return nil, validationError("The pending questions exceed the Web UI display limit. Answer them in the Codex terminal.")
	}
	sort.Slice(questions, func(i, j int) bool { return questions[i].ApprovalID < questions[j].ApprovalID })
	return questions, nil
}

func webUIPendingQuestion(pending pendingRequest) (webUIQuestion, bool) {
	var source map[string]json.RawMessage
	if json.Unmarshal(pending.request.Params, &source) != nil {
		return webUIQuestion{}, false
	}
	params := make(map[string]json.RawMessage)
	var fields []string
	switch pending.request.Method {
	case "item/tool/requestUserInput":
		questions := make([]map[string]any, 0, len(pending.request.Questions))
		for _, question := range pending.request.Questions {
			options := make([]map[string]string, 0, len(question.Choices))
			for _, option := range question.Choices {
				options = append(options, map[string]string{"label": option.Label, "description": option.Description})
			}
			questions = append(questions, map[string]any{"id": question.ID, "header": question.Header, "question": question.Prompt, "isOther": question.IsOther, "isSecret": question.IsSecret, "options": options})
		}
		params["questions"], _ = json.Marshal(questions)
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		fields = []string{"command", "reason", "cwd", "availableDecisions"}
	case "item/permissions/requestApproval":
		fields = []string{"permissions", "reason"}
	default:
		return webUIQuestion{}, false
	}
	for _, field := range fields {
		if source[field] != nil {
			params[field] = source[field]
		}
	}
	params["threadId"], _ = json.Marshal(pending.approval.ThreadID)
	params["turnId"], _ = json.Marshal(pending.approval.TurnID)
	params["itemId"], _ = json.Marshal(pending.approval.ItemID)
	return webUIQuestion{ApprovalID: pending.approval.ID, Method: pending.request.Method, Params: params}, true
}

func (s *sessionActor) webUIAnswerQuestion(ctx context.Context, client *codexadapter.Client, answer webUIQuestionAnswer) error {
	for requestID, pending := range s.pending {
		if pending.approval.ID != answer.ApprovalID || pending.approval.ThreadID != s.session.ThreadID || !client.RequestPending(requestID) || (pending.approval.TurnID != "" && pending.approval.TurnID != s.session.ActiveTurnID) {
			continue
		}
		question, ok := webUIPendingQuestion(pending)
		if !ok {
			return validationError("This approval type is not supported in the Web UI.")
		}
		validated, err := webUIAnswer(webUIRequest{method: question.Method, params: question.Params}, answer.Result, s.agent.redactor)
		if err != nil {
			return validationError("%s", err)
		}
		var answers map[string][]string
		state := "approved"
		switch question.Method {
		case "item/tool/requestUserInput":
			var payload struct {
				Answers map[string]struct {
					Answers []string `json:"answers"`
				} `json:"answers"`
			}
			_ = json.Unmarshal(validated, &payload)
			answers = make(map[string][]string, len(payload.Answers))
			for id, value := range payload.Answers {
				answers[id] = value.Answers
			}
			err = client.ReplyAnswers(ctx, requestID, answers)
		case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
			var payload struct {
				Decision string `json:"decision"`
			}
			_ = json.Unmarshal(validated, &payload)
			err = client.ReplyApproval(ctx, requestID, codexadapter.ApprovalResponse{Decision: payload.Decision})
			if payload.Decision == "decline" {
				state = "declined"
			}
			if payload.Decision == "cancel" {
				state = "cancelled"
			}
		default:
			err = client.Reply(ctx, pending.request, validated)
		}
		if err != nil {
			return err
		}
		if len(answers) > 0 {
			answered := pending.approval
			answered.Answers, answered.State = s.redactQuestionAnswers(answered, answers), "answered"
			if err := s.agent.emit(s.runtime, s.session.ID, "user_input_answered", answered); err != nil {
				return err
			}
		}
		s.resolve(requestID, state)
		return nil
	}
	return validationError("This question is no longer pending in the selected session.")
}
