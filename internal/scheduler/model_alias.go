// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"strings"

	"github.com/is7qin/c3api/internal/domain"
)

type modelAliasKey struct {
	format domain.RequestFormat
	alias  string
}

// buildModelAliases builds a route-local alias index. Model IDs are provider
// data, so aliases are intentionally narrow: only a short token such as k3 or
// a first-letter/version shorthand is considered, and collisions are removed.
// A request is never redirected when two advertised models could match it.
func buildModelAliases(routes map[routeKey]*route, upstreamRoutes map[routeKey]*upstreamRoute) map[modelAliasKey]string {
	candidates := make(map[modelAliasKey]string)
	ambiguous := make(map[modelAliasKey]struct{})
	add := func(format domain.RequestFormat, model string) {
		for _, alias := range modelShortAliases(model) {
			if alias == "" || alias == strings.ToLower(strings.TrimSpace(model)) {
				continue
			}
			key := modelAliasKey{format: format, alias: alias}
			if _, blocked := ambiguous[key]; blocked {
				continue
			}
			if previous, exists := candidates[key]; exists && previous != model {
				delete(candidates, key)
				ambiguous[key] = struct{}{}
				continue
			}
			candidates[key] = model
		}
	}
	for key := range routes {
		if key.model != "" {
			add(key.format, key.model)
		}
	}
	for key := range upstreamRoutes {
		if key.model != "" {
			add(key.format, key.model)
		}
	}
	return candidates
}

func modelShortAliases(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	lower := strings.ToLower(model)
	parts := strings.FieldsFunc(lower, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	if len(parts) < 2 {
		return nil
	}
	seen := make(map[string]struct{}, 4)
	aliases := make([]string, 0, 4)
	add := func(value string) {
		value = strings.TrimSpace(strings.ToLower(value))
		if len(value) < 2 || len(value) > 24 {
			return
		}
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		aliases = append(aliases, value)
	}
	// Provider IDs often contain a meaningful short token, e.g. kimi-k3 or
	// qwen-q3. Preserve only one-letter plus numeric tokens to avoid turning
	// ordinary dimensions such as 235b into an unrelated alias.
	for _, part := range parts {
		if shortModelToken(part) {
			add(part)
		}
	}
	// For names such as claude-3-7-sonnet, c3 and c37 are useful and still
	// deterministic. The alias is accepted only when unique within the group.
	first := ""
	firstNumber := ""
	var numbers strings.Builder
	for _, part := range parts {
		if first == "" && isAlphaToken(part) {
			first = part[:1]
		}
		if isNumericToken(part) && len(part) <= 2 {
			if firstNumber == "" {
				firstNumber = part
			}
			numbers.WriteString(part)
		}
	}
	if first != "" && numbers.Len() > 0 {
		add(first + firstNumber)
		add(first + numbers.String())
	}
	return aliases
}

func shortModelToken(value string) bool {
	return len(value) >= 2 && len(value) <= 4 && value[0] >= 'a' && value[0] <= 'z' && isNumericToken(value[1:])
}

func isAlphaToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 'a' || value[i] > 'z' {
			return false
		}
	}
	return true
}

func isNumericToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
