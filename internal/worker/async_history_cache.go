package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/workerdb"
)

const asyncHistoryFreshPages = 8
const asyncHistoryMaxPages = 10000

// readPendingQuestionHistory starts at the newest turn and stops after finding
// every request already saved by this worker. Old unrelated questions are
// never imported. A page budget limits reconciliation work; terminal older
// pages survive interruption/restart in SQLite, scoped to a fresh head digest.
func (s *sessionActor) readPendingQuestionHistory(ctx context.Context, client *codexadapter.Client, pending map[string]asyncQuestionRecord) ([]codexadapter.AsyncQuestion, error) {
	head, err := client.ReadHistoryPage(ctx, s.session.ThreadID, "", "desc")
	if err != nil {
		return nil, err
	}
	head = compactAsyncHistoryPage(head)
	encodedHead, err := json.Marshal(head)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encodedHead)
	prefix := asyncHistoryPrefix(s.runtime.ID, s.session.ThreadID)
	if err := s.agent.store.prepareAsyncHistoryCache(prefix, digest[:]); err != nil {
		return nil, err
	}
	remaining := make(map[string]bool, len(pending))
	for _, record := range pending {
		remaining[record.Approval.ItemID] = true
	}
	turns := make([]codexadapter.HistoryTurn, 0)
	seenTurns, seenCursors := make(map[string]bool), make(map[string]bool)
	freshReads, page := 1, head
	for index := 0; index < asyncHistoryMaxPages; index++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, turn := range page.Turns {
			if seenTurns[turn.ID] {
				return nil, fmt.Errorf("repeated question history turn: %w", codexadapter.ErrHistoryUnavailable)
			}
			seenTurns[turn.ID] = true
			turns = append(turns, turn)
			for _, event := range turn.QuestionEvents {
				if !event.IsInput && len(event.Questions) > 0 {
					delete(remaining, event.ItemID)
				}
			}
		}
		if len(remaining) == 0 || page.NextCursor == "" {
			for left, right := 0, len(turns)-1; left < right; left, right = left+1, right-1 {
				turns[left], turns[right] = turns[right], turns[left]
			}
			return codexadapter.ResolveHistoryQuestions(nil, turns), nil
		}
		cursor := page.NextCursor
		if seenCursors[cursor] {
			return nil, fmt.Errorf("repeated question history cursor: %w", codexadapter.ErrHistoryUnavailable)
		}
		seenCursors[cursor] = true
		cached, exists, err := s.agent.store.asyncHistoryCachedPage(prefix, cursor)
		if err != nil {
			return nil, err
		}
		if exists {
			page = cached
			continue
		}
		if freshReads >= asyncHistoryFreshPages {
			return nil, fmt.Errorf("question history reconciliation will continue: %w", codexadapter.ErrHistoryUnavailable)
		}
		page, err = client.ReadHistoryPage(ctx, s.session.ThreadID, cursor, "desc")
		freshReads++
		if err != nil {
			return nil, err
		}
		page = compactAsyncHistoryPage(page)
		if cacheableAsyncHistoryPage(page) {
			if err := s.agent.store.saveAsyncHistoryPage(prefix, cursor, page); err != nil {
				return nil, err
			}
		}
	}
	return nil, fmt.Errorf("question history reconciliation limit exceeded: %w", codexadapter.ErrHistoryUnavailable)
}

func compactAsyncHistoryPage(page codexadapter.HistoryTurnPage) codexadapter.HistoryTurnPage {
	for index := range page.Turns {
		turn := &page.Turns[index]
		turn.UserPrompts, turn.Messages = nil, nil
		turn.StartedAt, turn.CompletedAt = nil, nil
	}
	return page
}

func cacheableAsyncHistoryPage(page codexadapter.HistoryTurnPage) bool {
	for _, turn := range page.Turns {
		switch turn.Status {
		case "completed", "failed", "interrupted":
		default:
			return false
		}
	}
	return true
}

func asyncHistoryPrefix(runtimeID, threadID string) []byte {
	identity, _ := json.Marshal([]string{runtimeID, threadID})
	digest := sha256.Sum256(identity)
	return []byte("async_history:" + hex.EncodeToString(digest[:]) + ":")
}

func asyncHistoryPageKey(prefix []byte, cursor string) []byte {
	digest := sha256.Sum256([]byte(cursor))
	return []byte(string(prefix) + "page:" + hex.EncodeToString(digest[:]))
}

func (s *Store) prepareAsyncHistoryCache(prefix, digest []byte) error {
	return s.db.Update(func(tx *workerdb.Tx) error {
		bucket := tx.Bucket(bucketMeta)
		headKey := []byte(string(prefix) + "head")
		if bytes.Equal(bucket.Get(headKey), digest) {
			return nil
		}
		if err := purgeAsyncHistoryPrefix(tx, prefix); err != nil {
			return err
		}
		return bucket.Put(headKey, digest)
	})
}

func (s *Store) asyncHistoryCachedPage(prefix []byte, cursor string) (codexadapter.HistoryTurnPage, bool, error) {
	var page codexadapter.HistoryTurnPage
	found := false
	err := s.db.View(func(tx *workerdb.Tx) error {
		value := tx.Bucket(bucketMeta).Get(asyncHistoryPageKey(prefix, cursor))
		if value == nil {
			return nil
		}
		if err := json.Unmarshal(value, &page); err != nil {
			return err
		}
		found = true
		return nil
	})
	return page, found, err
}

func (s *Store) saveAsyncHistoryPage(prefix []byte, cursor string, page codexadapter.HistoryTurnPage) error {
	encoded, err := json.Marshal(page)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *workerdb.Tx) error {
		return tx.Bucket(bucketMeta).Put(asyncHistoryPageKey(prefix, cursor), encoded)
	})
}

func purgeAsyncHistoryCache(tx *workerdb.Tx, runtimeID, threadID string) error {
	return purgeAsyncHistoryPrefix(tx, asyncHistoryPrefix(runtimeID, threadID))
}

func purgeAsyncHistoryPrefix(tx *workerdb.Tx, prefix []byte) error {
	bucket := tx.Bucket(bucketMeta)
	var keys [][]byte
	cursor := bucket.Cursor()
	for key, _ := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
		keys = append(keys, append([]byte(nil), key...))
	}
	for _, key := range keys {
		if err := bucket.Delete(key); err != nil {
			return err
		}
	}
	return nil
}
