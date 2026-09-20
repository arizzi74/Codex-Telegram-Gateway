package worker

import (
	"context"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func (s *sessionActor) observeAsyncQuestion(event codexadapter.Event) {
	if s.session.Deleted || s.session.Archived || event.ItemID == "" || len(event.Questions) == 0 {
		return
	}
	approval := protocol.Approval{Async: true, RequestID: "async:" + event.ItemID, ThreadID: s.session.ThreadID, TurnID: event.TurnID, ItemID: event.ItemID, Type: "user_input", Summary: s.agent.redactor.Redact(event.Text), State: "pending"}
	for _, question := range event.Questions {
		q := protocol.Question{ID: question.ID, Prompt: question.Prompt, Header: question.Header, Secret: question.IsSecret}
		for _, choice := range question.Choices {
			q.Options = append(q.Options, choice.Label)
		}
		approval.Questions = append(approval.Questions, q)
	}
	s.rememberAsyncQuestion(approval)
}

func (s *sessionActor) rememberAsyncQuestion(approval protocol.Approval) {
	stored, pending, err := s.agent.store.announceAsyncQuestion(s.runtime, s.session.ID, approval)
	if err != nil {
		s.agent.report(err)
		return
	}
	if pending {
		if s.asyncQuestions == nil {
			s.asyncQuestions = make(map[string]protocol.Approval)
		}
		s.asyncQuestions[stored.RequestID] = stored
	}
}

// History only reconciles requests this worker already observed and saved.
// Codex 0.155 does not persist TUI-local dismissal, so importing unknown old
// requests would turn historical questions into new pending notifications.
// Reading history does not resume or otherwise change a thread.
func (s *sessionActor) recoverAsyncQuestions() {
	if s.session.Deleted || s.session.Archived || s.runtime.State != "running" || s.asyncRecoveredGeneration == s.runtime.Generation {
		return
	}
	records, err := s.agent.store.asyncQuestionRecords(s.session.ID)
	if err != nil {
		s.agent.report(err)
		return
	}
	pending := make(map[string]asyncQuestionRecord)
	for _, record := range records {
		if record.State == "pending" {
			pending[record.Approval.RequestID] = record
		}
	}
	if len(pending) == 0 {
		s.asyncQuestions = make(map[string]protocol.Approval)
		s.asyncRecoveredGeneration = s.runtime.Generation
		return
	}
	client, runtime, ok := s.agent.manager.Client(s.runtime.ID)
	if !ok || runtime.Generation != s.runtime.Generation {
		return
	}
	ctx, cancel := context.WithTimeout(s.agent.ctx, 10*time.Second)
	defer cancel()
	history, historyErr := s.readPendingQuestionHistory(ctx, client, pending)
	if historyErr == nil {
		for _, item := range history {
			requestID := "async:" + item.ItemID
			record, exists := pending[requestID]
			if !exists {
				continue
			}
			// Intersect history with the saved remaining fields. History must
			// never restore a field answered or dismissed through Telegram.
			remaining := make([]protocol.Question, 0, len(record.Approval.Questions))
			state := "resolved"
			for _, question := range record.Approval.Questions {
				if containsString(item.SupersededIDs, question.ID) {
					state = "superseded"
					continue
				}
				if !containsString(item.AnsweredIDs, question.ID) {
					remaining = append(remaining, question)
				}
			}
			if len(remaining) == 0 {
				// The old runtime's gateway requests are already cleared on a
				// generation change; otherwise clear the existing notification.
				if err := s.agent.store.setAsyncQuestionState(s.runtime, s.session.ID, requestID, state, record.Generation == s.runtime.Generation); err != nil {
					s.agent.report(err)
					return
				}
				delete(pending, requestID)
				continue
			}
			record.Approval.Questions = remaining
			pending[requestID] = record
		}
	}
	s.asyncQuestions = make(map[string]protocol.Approval)
	for _, record := range pending {
		s.rememberAsyncQuestion(record.Approval)
	}
	if historyErr != nil {
		// A failed or incomplete history read cannot prove that a durable
		// prompt was answered. Preserve it and retry the read next snapshot.
		s.agent.log.Debug("async question history unavailable", "session_id", s.session.ID, "error", historyErr)
		return
	}
	s.asyncRecoveredGeneration = s.runtime.Generation
}

func (s *sessionActor) observeAsyncAnswer(text string) {
	supersedes := codexadapter.AsyncQuestionInputSupersedes(text)
	for requestID, approval := range s.asyncQuestions {
		remaining := make([]protocol.Question, 0, len(approval.Questions))
		for _, question := range approval.Questions {
			if !supersedes && !codexadapter.AsyncQuestionAnswerMatches(question.Prompt, text) {
				remaining = append(remaining, question)
			}
		}
		if len(remaining) == len(approval.Questions) {
			continue
		}
		if len(remaining) == 0 {
			state := "resolved"
			if supersedes {
				state = "superseded"
			}
			if err := s.agent.store.setAsyncQuestionState(s.runtime, s.session.ID, requestID, state, true); err != nil {
				s.agent.report(err)
				return
			}
			delete(s.asyncQuestions, requestID)
		} else {
			approval.Questions = remaining
			s.rememberAsyncQuestion(approval)
		}
	}
}

func (s *sessionActor) answerAsyncQuestion(command protocol.Command, client *codexadapter.Client, approval protocol.Approval) {
	if command.Arguments.Decision == "dismiss" && len(command.Arguments.Answers) == 0 {
		if err := s.agent.store.setAsyncQuestionState(s.runtime, s.session.ID, approval.RequestID, "resolved", true); err != nil {
			s.agent.report(err)
			return
		}
		delete(s.asyncQuestions, approval.RequestID)
		_, err := s.agent.record(command, CommandCompleted, &protocol.Result{CommandID: command.ID, State: "completed"}, "command_completed")
		s.agent.report(err)
		return
	}
	var parts []string
	for _, question := range approval.Questions {
		values := command.Arguments.Answers[question.ID]
		if len(values) != 1 || strings.TrimSpace(values[0]) == "" || command.Arguments.Decision != "" {
			_, err := s.agent.reject(command, protocol.ApprovalNotPending, "The question or its answers changed. Open /tgquestions again.")
			s.agent.report(err)
			return
		}
		parts = append(parts, codexadapter.FormatAsyncQuestionAnswer(question.Prompt, values[0]))
	}
	if _, err := canonicalWorkspace(s.session.CWD, s.agent.cfg.AllowedWorkspaceRoots); err != nil {
		s.agent.report(s.agent.executionError(command, validationError("session workspace is not allowed")))
		return
	}
	if s.session.ActiveTurnID == "" {
		if err := s.ensureCodexThreadLoaded(s.agent.ctx, client); err != nil {
			// Loading may fail, but no answer has been submitted yet.
			s.agent.report(s.agent.executionError(command, validationError("Could not load the question's session. Open /tgquestions and retry once the session is available.")))
			return
		}
	}
	if err := s.agent.store.setAsyncQuestionState(s.runtime, s.session.ID, approval.RequestID, "submitting", false); err != nil {
		s.agent.report(err)
		return
	}
	text := strings.Join(parts, "\n\n")
	var err error
	started := false
	var turn codexadapter.Turn
	if s.session.ActiveTurnID != "" {
		turn, err = client.Steer(s.agent.ctx, s.session.ThreadID, s.session.ActiveTurnID, text)
	} else {
		turn, err = client.StartTurn(s.agent.ctx, s.session.ThreadID, text)
		started = err == nil
	}
	if err != nil {
		if definiteRPCFailure(err) {
			s.agent.report(s.agent.store.setAsyncQuestionState(s.runtime, s.session.ID, approval.RequestID, "pending", false))
		} else {
			s.agent.report(s.agent.store.setAsyncQuestionState(s.runtime, s.session.ID, approval.RequestID, "outcome_unknown", true))
			delete(s.asyncQuestions, approval.RequestID)
		}
		s.agent.report(s.agent.executionError(command, err))
		return
	}
	if err := s.agent.store.setAsyncQuestionState(s.runtime, s.session.ID, approval.RequestID, "resolved", true); err != nil {
		s.agent.report(err)
		return
	}
	delete(s.asyncQuestions, approval.RequestID)
	result := &protocol.Result{CommandID: command.ID, State: "completed"}
	kind := "command_completed"
	if started {
		s.session.ActiveTurnID, s.session.State, s.session.Loaded = turn.ID, "running", true
		s.activeCommand = &command
		s.resetMessages()
		s.save()
		result.State, result.TurnID, kind = "running", turn.ID, "turn_started"
	}
	_, err = s.agent.record(command, CommandCompleted, result, kind)
	s.agent.report(err)
}
