package registry

import "context"

// TelegramBotQuestionPreference is independent of refreshed identity, profile
// names and credentials. Missing preference rows always mean broadcast is off.
type TelegramBotQuestionPreference struct {
	BotID              string
	BroadcastQuestions bool
}

func (s *Store) TelegramBotQuestionPreferences(ctx context.Context) ([]TelegramBotQuestionPreference, error) {
	rows, err := s.pool.Query(ctx, `SELECT bot.bot_id,COALESCE(preference.broadcast_questions,0)
        FROM telegram_bots bot LEFT JOIN telegram_bot_question_preferences preference USING(bot_id)
        ORDER BY bot.bot_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TelegramBotQuestionPreference{}
	for rows.Next() {
		var preference TelegramBotQuestionPreference
		if err := rows.Scan(&preference.BotID, &preference.BroadcastQuestions); err != nil {
			return nil, err
		}
		out = append(out, preference)
	}
	return out, rows.Err()
}

func (s *Store) SetTelegramBotBroadcastQuestions(ctx context.Context, id string, enabled bool) error {
	tag, err := s.pool.Exec(ctx, `INSERT INTO telegram_bot_question_preferences(bot_id,broadcast_questions)
        SELECT bot_id,$2 FROM telegram_bots WHERE bot_id=$1
        ON CONFLICT(bot_id) DO UPDATE SET broadcast_questions=excluded.broadcast_questions`, id, enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrTelegramBotRecordNotFound
	}
	return nil
}

// telegramQuestionBotAllowedSQL shares the WebUI's owner-private connection
// definition. Groups, topics and other users never select the source bot. Old
// databases may contain multiple owner-private connections; the first bot ID
// wins deterministically, and only that bot's preference can broaden delivery.
// Bot records may be absent for legacy callers, as in ordinary Telegram routing.
func telegramQuestionBotAllowedSQL(session, destinationBot string) string {
	return `(EXISTS(SELECT 1 FROM sessions question_session
        JOIN workers question_worker ON question_worker.worker_id=question_session.worker_id AND question_worker.enabled=1
        WHERE question_session.session_id=` + session + ` AND question_session.archived=0)
        AND NOT EXISTS(SELECT 1 FROM telegram_bots destination WHERE destination.bot_id=` + destinationBot + ` AND destination.enabled=0)
        AND NOT EXISTS(SELECT 1 FROM telegram_bot_retirements destination_retired WHERE destination_retired.bot_id=` + destinationBot + `)
        AND COALESCE((SELECT bot.bot_id=` + destinationBot + ` OR COALESCE(preference.broadcast_questions,0)=1
            FROM telegram_bots bot JOIN telegram_bindings binding ON binding.bot_id=bot.bot_id
            JOIN sessions session ON session.session_id=binding.session_id AND session.archived=0
            JOIN workers worker ON worker.worker_id=session.worker_id AND worker.enabled=1
            LEFT JOIN telegram_bot_question_preferences preference ON preference.bot_id=bot.bot_id
            WHERE binding.session_id=` + session + ` AND ` + telegramOwnerPrivateBindingSQL("bot", "binding") + `
              AND ` + webUITelegramBotAvailableSQL + telegramChatAllowedSQL("bot.bot_id", "bot.owner_id") + `
            ORDER BY bot.bot_id LIMIT 1),1)=1)`
}

// Partial-answer broadcasts have no initiating user; direct replies and manual
// /tgquestions prompts retain their authenticated user and existing access.
// This also recognizes automatically queued followups from before migration.
const automaticQuestionDeliverySQL = `(delivery.kind IN ('approval_requested','user_input_requested')
    OR (delivery.kind='ui_response' AND COALESCE(json_extract(delivery.payload,'$.view'),'')='input_pending'
        AND COALESCE(json_extract(delivery.payload,'$.user_id'),0)=0))`

func automaticQuestionDeliveryAllowedSQL() string {
	return `(NOT ` + automaticQuestionDeliverySQL + ` OR ` + telegramQuestionBotAllowedSQL(
		`CASE WHEN delivery.kind='ui_response' THEN json_extract(delivery.payload,'$.session_id')
            ELSE (SELECT question_event.session_id FROM events question_event WHERE question_event.event_id=delivery.event_id) END`, "delivery.bot_id") + `)`
}
