// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package protoconv

import "encoding/json"

// reasoning/thinking controls have different wire shapes. These helpers keep
// the conversion deterministic and conservative: the intent to expose a
// provider's reasoning summary is preserved, while provider-specific budget
// knobs are translated to the nearest portable effort level.

func thinkingToReasoning(value any) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	if !ok || m == nil {
		return nil, false
	}
	if typ, _ := m["type"].(string); typ == "disabled" {
		return nil, false
	}
	effort := "medium"
	if budget, ok := reasoningBudget(m["budget_tokens"]); ok {
		switch {
		case budget <= 1024:
			effort = "low"
		case budget >= 4096:
			effort = "high"
		}
	}
	return map[string]any{"effort": effort, "summary": "auto"}, true
}

func reasoningBudget(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	default:
		return 0, false
	}
}

func reasoningToThinking(value any) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	if !ok || m == nil {
		return nil, false
	}
	if effort, _ := m["effort"].(string); effort != "" {
		budget := 2048
		switch effort {
		case "low":
			budget = 1024
		case "high":
			budget = 4096
		}
		return map[string]any{"type": "enabled", "budget_tokens": budget}, true
	}
	if typ, _ := m["type"].(string); typ == "enabled" {
		return map[string]any{"type": "enabled", "budget_tokens": 2048}, true
	}
	return nil, false
}
