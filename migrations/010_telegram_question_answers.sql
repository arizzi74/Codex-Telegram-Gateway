-- Keep original wording after a worker removes answered questions from a request.
CREATE TABLE telegram_question_answers (
    approval_id TEXT NOT NULL REFERENCES approvals(approval_id),
    question_id TEXT NOT NULL CHECK (question_id <> ''),
    question TEXT NOT NULL CHECK (json_valid(question)),
    answer TEXT,
    version INTEGER NOT NULL DEFAULT 0 CHECK (version >= 0),
    PRIMARY KEY (approval_id, question_id)
) STRICT;

INSERT OR IGNORE INTO telegram_question_answers(approval_id,question_id,question)
    SELECT approval.approval_id,json_extract(question.value,'$.id'),question.value
    FROM approvals approval,json_each(approval.request_payload,'$.questions') question
    WHERE json_type(question.value,'$.id')='text' AND json_extract(question.value,'$.id')<>'';

CREATE INDEX bot_message_routes_question_idx
    ON bot_message_routes(approval_id,question_id) WHERE approval_id IS NOT NULL;

-- Match old edit deliveries directly when a definitely failed response is retried.
CREATE TABLE telegram_question_answer_edits (
    delivery_id TEXT NOT NULL PRIMARY KEY REFERENCES telegram_deliveries(delivery_id),
    approval_id TEXT NOT NULL,
    question_id TEXT NOT NULL,
    version INTEGER NOT NULL,
    FOREIGN KEY (approval_id, question_id) REFERENCES telegram_question_answers(approval_id, question_id)
) STRICT;
CREATE INDEX telegram_question_answer_edits_question_idx
    ON telegram_question_answer_edits(approval_id,question_id,version);
