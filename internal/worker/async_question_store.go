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
		encoded, err := json.Marshal(asyncQuestionRecord{Approval: approval, Generation: runtime.Generation, State: "pending"})
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
		_, err = appendEvent(tx, protocol.Event{RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: sessionID, Kind: "user_input_requested", Data: data})
		pending = err == nil
		return err
	})
	return approval, pending, err
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
			_, err = appendEvent(tx, protocol.Event{RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: sessionID, Kind: "approval_resolved", Data: data})
			return err
		}
		return nil
	})
}
