-- Chunk checkpoints let a long Telegram response resume after a crash without
-- resending chunks whose Telegram message IDs were already committed.
CREATE TABLE telegram_delivery_chunks (
    delivery_id UUID NOT NULL REFERENCES telegram_deliveries(delivery_id) ON DELETE CASCADE,
    chunk_index INTEGER NOT NULL CHECK (chunk_index >= 0),
    payload JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sent')),
    telegram_message_id BIGINT,
    sent_at TIMESTAMPTZ,
    PRIMARY KEY (delivery_id, chunk_index)
);

CREATE INDEX telegram_delivery_chunks_pending_idx
    ON telegram_delivery_chunks (delivery_id, chunk_index) WHERE status = 'pending';
