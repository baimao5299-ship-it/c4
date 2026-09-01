// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// requestJSONKind describes the JSON types that a top-level request field may
// contain. The SDK parameter decoders intentionally accept loose coercions
// (for example a numeric string for an integer field); gateway requests need a
// strict wire-type check so malformed requests never consume an upstream slot.
type requestJSONKind uint8

const (
	requestJSONNull requestJSONKind = 1 << iota
	requestJSONString
	requestJSONBool
	requestJSONNumber
	requestJSONInteger
	requestJSONObject
	requestJSONArray
)

type requestFieldSpec struct {
	key   []byte
	kinds requestJSONKind
	label string
}

type requestFieldSet struct {
	specs []requestFieldSpec
	keys  [][]byte
}

func newRequestFieldSet(specs []requestFieldSpec) requestFieldSet {
	keys := make([][]byte, len(specs))
	for i := range specs {
		keys[i] = specs[i].key
	}
	return requestFieldSet{specs: specs, keys: keys}
}

func requestField(key, label string, kinds requestJSONKind) requestFieldSpec {
	return requestFieldSpec{key: []byte(key), kinds: kinds | requestJSONNull, label: label}
}

var (
	chatRequestFields = newRequestFieldSet([]requestFieldSpec{
		requestField("messages", "an array", requestJSONArray),
		requestField("model", "a string", requestJSONString),
		requestField("frequency_penalty", "a number", requestJSONNumber),
		requestField("logprobs", "a boolean", requestJSONBool),
		requestField("max_completion_tokens", "an integer", requestJSONInteger),
		requestField("max_tokens", "an integer", requestJSONInteger),
		requestField("n", "an integer", requestJSONInteger),
		requestField("presence_penalty", "a number", requestJSONNumber),
		requestField("seed", "an integer", requestJSONInteger),
		requestField("store", "a boolean", requestJSONBool),
		requestField("temperature", "a number", requestJSONNumber),
		requestField("top_logprobs", "an integer", requestJSONInteger),
		requestField("top_p", "a number", requestJSONNumber),
		requestField("parallel_tool_calls", "a boolean", requestJSONBool),
		requestField("prompt_cache_key", "a string", requestJSONString),
		requestField("safety_identifier", "a string", requestJSONString),
		requestField("user", "a string", requestJSONString),
		requestField("audio", "an object", requestJSONObject),
		requestField("logit_bias", "an object", requestJSONObject),
		requestField("metadata", "an object", requestJSONObject),
		requestField("modalities", "an array", requestJSONArray),
		requestField("reasoning_effort", "a string", requestJSONString),
		requestField("service_tier", "a string", requestJSONString),
		requestField("stop", "a string or array", requestJSONString|requestJSONArray),
		requestField("stream", "a boolean", requestJSONBool),
		requestField("stream_options", "an object", requestJSONObject),
		requestField("function_call", "a string or object", requestJSONString|requestJSONObject),
		requestField("functions", "an array", requestJSONArray),
		requestField("prediction", "an object", requestJSONObject),
		requestField("response_format", "an object", requestJSONObject),
		requestField("tool_choice", "a string or object", requestJSONString|requestJSONObject),
		requestField("tools", "an array", requestJSONArray),
		requestField("web_search_options", "an object", requestJSONObject),
	})
	responsesRequestFields = newRequestFieldSet([]requestFieldSpec{
		requestField("background", "a boolean", requestJSONBool),
		requestField("instructions", "a string", requestJSONString),
		requestField("max_output_tokens", "an integer", requestJSONInteger),
		requestField("max_tool_calls", "an integer", requestJSONInteger),
		requestField("parallel_tool_calls", "a boolean", requestJSONBool),
		requestField("previous_response_id", "a string", requestJSONString),
		requestField("store", "a boolean", requestJSONBool),
		requestField("temperature", "a number", requestJSONNumber),
		requestField("top_logprobs", "an integer", requestJSONInteger),
		requestField("top_p", "a number", requestJSONNumber),
		requestField("prompt_cache_key", "a string", requestJSONString),
		requestField("safety_identifier", "a string", requestJSONString),
		requestField("include", "an array", requestJSONArray),
		requestField("metadata", "an object", requestJSONObject),
		requestField("prompt", "an object", requestJSONObject),
		requestField("service_tier", "a string", requestJSONString),
		requestField("truncation", "a string", requestJSONString),
		requestField("input", "a string or array", requestJSONString|requestJSONArray),
		requestField("model", "a string", requestJSONString),
		requestField("reasoning", "an object", requestJSONObject),
		requestField("text", "an object", requestJSONObject),
		requestField("tool_choice", "a string or object", requestJSONString|requestJSONObject),
		requestField("tools", "an array", requestJSONArray),
		requestField("stream", "a boolean", requestJSONBool),
	})
	anthropicRequestFields = newRequestFieldSet([]requestFieldSpec{
		requestField("max_tokens", "an integer", requestJSONInteger),
		requestField("messages", "an array", requestJSONArray),
		requestField("model", "a string", requestJSONString),
		requestField("container", "a string", requestJSONString),
		requestField("inference_geo", "a string", requestJSONString),
		requestField("temperature", "a number", requestJSONNumber),
		requestField("top_k", "an integer", requestJSONInteger),
		requestField("top_p", "a number", requestJSONNumber),
		requestField("cache_control", "an object", requestJSONObject),
		requestField("metadata", "an object", requestJSONObject),
		requestField("output_config", "an object", requestJSONObject),
		requestField("service_tier", "a string", requestJSONString),
		requestField("stop_sequences", "an array", requestJSONArray),
		// Anthropic accepts the documented string shorthand as well as the
		// structured text-block array. The non-streaming SDK may normalize the
		// shorthand differently, but rejecting it here would break converted
		// streaming requests before they reach the compatible upstream.
		requestField("system", "a string or array", requestJSONString|requestJSONArray),
		requestField("thinking", "an object", requestJSONObject),
		requestField("tool_choice", "an object", requestJSONObject),
		requestField("tools", "an array", requestJSONArray),
		requestField("stream", "a boolean", requestJSONBool),
	})
)

func requestFieldsFor(format domain.RequestFormat) requestFieldSet {
	switch format {
	case domain.FormatOpenAIChat:
		return chatRequestFields
	case domain.FormatOpenAIResponses:
		return responsesRequestFields
	case domain.FormatAnthropic:
		return anthropicRequestFields
	default:
		return requestFieldSet{}
	}
}

// validateRequestParameterTypes validates known top-level fields while leaving
// provider-specific extension fields untouched. Missing fields and explicit
// null retain the SDK's existing semantics; only a present, non-null value with
// the wrong JSON wire type is rejected.
func validateRequestParameterTypes(format domain.RequestFormat, body []byte) error {
	set := requestFieldsFor(format)
	if len(set.specs) == 0 {
		return nil
	}
	if !isJSONObjectRoot(body) {
		return errBodyNotObject
	}
	var values [40][]byte
	scanKeys(body, set.keys, values[:len(set.keys)])
	for i, spec := range set.specs {
		raw := values[i]
		if len(raw) == 0 {
			continue
		}
		kind := requestJSONValueKind(raw)
		if requestJSONKindMatches(kind, spec.kinds, raw) {
			continue
		}
		return fmt.Errorf("%s must be %s", string(spec.key), spec.label)
	}
	return nil
}

func requestJSONValueKind(raw []byte) requestJSONKind {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0
	}
	switch raw[0] {
	case 'n':
		return requestJSONNull
	case '"':
		return requestJSONString
	case 't', 'f':
		return requestJSONBool
	case '{':
		return requestJSONObject
	case '[':
		return requestJSONArray
	default:
		return requestJSONNumber
	}
}

func requestJSONKindMatches(kind, allowed requestJSONKind, raw []byte) bool {
	if kind == requestJSONNumber {
		if allowed&requestJSONInteger != 0 && isJSONInteger(raw) {
			return true
		}
		return allowed&requestJSONNumber != 0
	}
	return allowed&kind != 0
}

func isJSONInteger(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.ContainsAny(raw, ".eE") {
		return false
	}
	_, err := strconv.ParseInt(string(raw), 10, 64)
	return err == nil
}

// rejectLocalRequest finishes a request that was rejected after account
// selection but before any provider call. Keeping this path shared guarantees
// that strict schema failures release the selected scheduler slot and retain
// the same detailed error-log attribution as SDK decode failures.
func (p *Proxy) rejectLocalRequest(ctx context.Context, w http.ResponseWriter, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, format domain.RequestFormat, body []byte, err error) (int, []byte, bool, error) {
	message := "invalid request body: " + err.Error()
	reqModel := gjson.GetBytes(body, "model").String()
	p.recordRejected(ctx, reqID, groupID, sel.AccountID, reqModel, sel.Model,
		format, http.StatusBadRequest, domain.Err4xx, 0, usageTuple{}, start,
		localRejectionMessage("invalid request body", err), sel)
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
		"message": message,
	}})
	p.sched.ReleaseSelection(sel)
	return http.StatusBadRequest, nil, true, nil
}
