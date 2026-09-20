-- Keep per-send selection barriers independent of the size of sent history.
CREATE INDEX telegram_selection_confirmation_idx ON telegram_deliveries (
    bot_id,chat_id,message_thread_id,json_extract(payload,'$.session_id')
) WHERE kind='ui_response' AND json_extract(payload,'$.view')='selected'
    AND status IN ('pending','failed','sending') AND visibility_revoked=0;
