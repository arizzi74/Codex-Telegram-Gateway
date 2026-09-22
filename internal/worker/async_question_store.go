package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
)

var bucketAsyncQuestions = []byte("async_questions")

type asyncQuestionRecord struct {
	Approval   protocol.Approval `json:"approval"`
	Generation uint64            `json:"generation"`
	State      string            `json:"state"`
	// AnsweredIDs fences replay between publishing an answer and applying the
	// remaining-question state. Only redacted answer text enters the outbox.
	AnsweredIDs []string `json:"answered_ids,omitempty"`
}

func asyncQuestionKey(sessionID, requestID string) []byte {
	return []byte(sessionID + "\x00" + requestID)
}

// Persist the question and its notification together. Reconciliation cannot
// reopen an answered question or duplicate a notification after reconnecting.
func (s *Store) announceAsyncQuestion(runtime protocol.Runtime, sessionID string, approval protocol.Approval) (protocol.Approval, bool, error) {
	var pending bool
	err := s.db.Update(func(tx *workerdb.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketAsyncQuestions)
		if err != nil {
			return err
		}
		key := asyncQuestionKey(sessionID, approval.RequestID)
		var saved asyncQuestionRecord
		if value := b.Get(key); value != nil {
			if err := json.Unmarshal(value, &saved); err != nil {
				return err
			}
			if saved.State != "pending" {
				return nil
			}
			if saved.Generation == runtime.Generation && reflect.DeepEqual(saved.Approval.Questions, approval.Questions) {
				approval, pending = saved.Approval, true
				return nil
			}
		}
		approval.ID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%s/%d/%s/%s", runtime.ID, runtime.Generation, sessionID, approval.RequestID))).String()
		approval.Async, approval.State = true, "pending"
		encoded, err := json.Marshal(asyncQuestionRecord{Approval: approval, Generation: runtime.Generation, State: "pending", AnsweredIDs: saved.AnsweredIDs})
		if err != nil {
			return err
		}
		if err := b.Put(key, encoded); err != nil {
			return err
		}
		data, err := json.Marshal(approval)
		if err != nil {
			return err
		}
		_, err = s.appendEvent(tx, protocol.Event{RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: sessionID, Kind: "user_input_requested", Data: data})
		pending = err == nil
		return err
	})
	return approval, pending, err
}

// recordAsyncQuestionAnswers durably emits actual answers once, before the
// request shrinks or resolves. The original stored questions and callback ID
// tie the event to the existing Telegram question, never a replacement one.
func (s *Store) recordAsyncQuestionAnswers(runtime protocol.Runtime, sessionID, requestID string, answers map[string][]string) error {
	if len(answers) == 0 {
		return nil
	}
	return s.db.Update(func(tx *workerdb.Tx) error {
		bucket := tx.Bucket(bucketAsyncQuestions)
		if bucket == nil {
			return fmt.Errorf("async question is unavailable")
		}
		key := asyncQuestionKey(sessionID, requestID)
		var saved asyncQuestionRecord
		if err := json.Unmarshal(bucket.Get(key), &saved); err != nil {
			return err
		}
		if saved.State != "pending" && saved.State != "submitting" {
			return nil
		}
		approval := saved.Approval
		approval.Questions = nil
		approval.Answers = make(map[string][]string)
		approval.State = "answered"
		for _, question := range saved.Approval.Questions {
			values := answers[question.ID]
			if len(values) == 0 || containsString(saved.AnsweredIDs, question.ID) {
				continue
			}
			approval.Questions = append(approval.Questions, question)
			approval.Answers[question.ID] = values
			saved.AnsweredIDs = append(saved.AnsweredIDs, question.ID)
		}
		if len(approval.Questions) == 0 {
			return nil
		}
		data, err := json.Marshal(approval)
		if err != nil {
			return err
		}
		// History can recover an answer after a runtime restart. Keep the
		// original generation and approval identity so the gateway edits only
		// that historical question, never a current request with reused IDs.
		if _, err := s.appendEvent(tx, protocol.Event{RuntimeID: runtime.ID, RuntimeGeneration: saved.Generation, SessionID: sessionID, Kind: "user_input_answered", Data: data}); err != nil {
			return err
		}
		encoded, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		return bucket.Put(key, encoded)
	})
}

func (s *Store) asyncQuestionRecords(sessionID string) ([]asyncQuestionRecord, error) {
	var records []asyncQuestionRecord
	err := s.db.View(func(tx *workerdb.Tx) error {
		b := tx.Bucket(bucketAsyncQuestions)
		if b == nil {
			return nil
		}
		prefix := []byte(sessionID + "\x00")
		c := b.Cursor()
		for key, value := c.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, value = c.Next() {
			var record asyncQuestionRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			records = append(records, record)
		}
		return nil
	})
	return records, err
}

func (s *Store) setAsyncQuestionState(runtime protocol.Runtime, sessionID, requestID, state string, resolve bool) error {
	return s.db.Update(func(tx *workerdb.Tx) error {
		b := tx.Bucket(bucketAsyncQuestions)
		if b == nil {
			return fmt.Errorf("async question is unavailable")
		}
		key := asyncQuestionKey(sessionID, requestID)
		var saved asyncQuestionRecord
		if err := json.Unmarshal(b.Get(key), &saved); err != nil {
			return err
		}
		saved.State = state
		encoded, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		if err := b.Put(key, encoded); err != nil {
			return err
		}
		if resolve {
			saved.Approval.State = "cleared"
			data, err := json.Marshal(saved.Approval)
			if err != nil {
				return err
			}
			_, err = s.appendEvent(tx, protocol.Event{RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: sessionID, Kind: "approval_resolved", Data: data})
			return err
		}
		return nil
	})
}
