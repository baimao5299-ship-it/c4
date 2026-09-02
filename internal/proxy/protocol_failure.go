// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license
// (closed-source deployment exemption); see LICENSE and LICENSE.commercial.

package proxy

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/pkg/sserelay"
)

// convertedStreamError represents an application-level failure announced by
// an upstream SSE stream after the HTTP response has started. It implements
// StatusCode so the normal stream-abort path classifies it as a 502/5xx event,
// while the caller still returns handled=true and never retries the request.
type convertedStreamError struct {
	message string
}

func (e *convertedStreamError) Error() string {
	if e == nil || strings.TrimSpace(e.message) == "" {
		return "upstream stream reported a failure"
	}
	return "upstream stream reported a failure: " + e.message
}

func (e *convertedStreamError) StatusCode() int { return 502 }

// convertedStreamFailure identifies terminal failure events before a converted
// stream is marked successful. Providers vary between named events
// (response.failed/error) and data-only JSON envelopes, so both forms are
// recognized. Only envelope-level fields are inspected; nested tool content is
// not treated as a transport failure.
func convertedStreamFailure(ev sserelay.Event) error {
	name := strings.ToLower(strings.TrimSpace(string(ev.EventName())))
	data := bytes.TrimSpace(ev.Data)
	if bytes.Equal(data, []byte("[DONE]")) {
		return nil
	}
	if name != "response.failed" && name != "response.error" && name != "error" {
		if !streamFailureEnvelope(data) {
			return nil
		}
	}
	message := streamFailureMessage(data)
	return &convertedStreamError{message: message}
}

func streamFailureEnvelope(data []byte) bool {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || root == nil {
		return false
	}
	if raw, ok := root["error"]; ok && hasJSONErrorPayload(raw) {
		return true
	}
	if failureStatus(rawText(root["status"])) || strings.EqualFold(rawText(root["type"]), "error") {
		return true
	}
	if raw, ok := root["response"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil && nested != nil {
			if failureStatus(rawText(nested["status"])) {
				return true
			}
			if errRaw, exists := nested["error"]; exists && hasJSONErrorPayload(errRaw) {
				return true
			}
		}
	}
	return false
}

func streamFailureMessage(data []byte) string {
	for _, path := range []string{
		"error.message",
		"response.error.message",
		"message",
		"response.message",
	} {
		if value := strings.TrimSpace(gjson.GetBytes(data, path).String()); value != "" {
			return value
		}
	}
	return "upstream stream reported a terminal failure"
}

func jsonNull(raw json.RawMessage) bool {
	return strings.EqualFold(strings.TrimSpace(string(raw)), "null")
}

func hasJSONErrorPayload(raw json.RawMessage) bool {
	if len(raw) == 0 || jsonNull(raw) {
		return false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for _, value := range obj {
			if jsonNull(value) {
				continue
			}
			var text string
			if json.Unmarshal(value, &text) == nil {
				if strings.TrimSpace(text) != "" {
					return true
				}
				continue
			}
			return len(value) > 0 && strings.TrimSpace(string(value)) != "{}"
		}
		return false
	}
	return strings.TrimSpace(string(raw)) != ""
}

func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return ""
}

func failureStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "failure", "cancelled", "canceled", "error", "errored", "rejected":
		return true
	default:
		return false
	}
}
