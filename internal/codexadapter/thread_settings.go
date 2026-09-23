package codexadapter

import (
	"encoding/json"

	"github.com/iaia/telegramgw/internal/protocol"
)

// CurrentThreadSettings is the small public preference snapshot broadcast by
// the native server. It never derives settings from a historical turn or
// exposes the notification's permissions, filesystem paths or other fields.
type CurrentThreadSettings struct {
	Model           string
	ReasoningEffort string
}

func decodeCurrentThreadSettings(raw json.RawMessage) *CurrentThreadSettings {
	var notification struct {
		Settings *struct {
			Model  string  `json:"model"`
			Effort *string `json:"effort"`
		} `json:"threadSettings"`
	}
	if json.Unmarshal(raw, &notification) != nil || notification.Settings == nil || !protocol.ModelMenuToken(notification.Settings.Model) {
		return nil
	}
	settings := &CurrentThreadSettings{Model: notification.Settings.Model}
	if notification.Settings.Effort != nil {
		settings.ReasoningEffort = *notification.Settings.Effort
		if settings.ReasoningEffort != "" && !protocol.ModelMenuToken(settings.ReasoningEffort) {
			return nil
		}
	}
	return settings
}
