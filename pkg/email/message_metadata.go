package email

import (
	"encoding/json"
	"strings"
)

// MessageMetadata preserves addressing for this email independently of its thread.
type MessageMetadata struct {
	From        string   `json:"from"`
	To          []string `json:"to"`
	Cc          []string `json:"cc"`
	DeliveredTo []string `json:"delivered_to"`
}

func messageMetadata(parsed *ParsedEmail) *MessageMetadata {
	addresses := func(values []string) []string {
		out := make([]string, 0, len(values))
		for _, value := range values {
			if address := extractEmailAddress(value); address != "" {
				out = append(out, strings.ToLower(address))
			}
		}
		return out
	}
	return &MessageMetadata{From: strings.ToLower(extractEmailAddress(parsed.From)), To: addresses(parsed.To), Cc: addresses(parsed.Cc), DeliveredTo: addresses(parsed.RawDeliveredTo)}
}

func (m MessageMetadata) MarshalJSON() ([]byte, error) {
	if m.From == "" && m.To == nil && m.Cc == nil && m.DeliveredTo == nil {
		return []byte("{}"), nil
	}
	type fields MessageMetadata
	return json.Marshal(fields(m))
}
