// Package safeguard recognizes Claude policy refusals in failed usage records.
//
// When Claude stops a response with stop_reason "refusal" (or stop_details of
// type refusal, or a refusal category such as cyber), CLIProxyAPI turns the
// request into a 400 error whose text is built by
// clienterror.ClaudePolicyRefusalMessage:
//
//	claude refusal (<category>): <explanation>
//	claude refusal: <explanation>
//	claude refusal (<category>)
//	claude refusal: <raw stop_details>
//	claude refusal
//
// The usage record of that request arrives with Failed set, Failure.StatusCode
// 400 and that text in Failure.Body, for streaming and non-streaming requests
// alike. clienterror.IsClaudePolicyRefusalError also treats a Messages JSON
// body whose stop_details describe a refusal, and Anthropic's plain-text
// cyber usage policy rejection, as refusals; this package does the same.
package safeguard

import (
	"encoding/json"
	"strings"
)

// Refusal is what a refused request's failure says.
type Refusal struct {
	// Category is the refusal category, such as cyber; "" when unnamed.
	Category string
	// Reason is the provider's explanation; "" when none was given.
	Reason string
}

// refusalCategories are the stop_details categories CLIProxyAPI treats as
// refusals even without a refusal stop reason.
var refusalCategories = map[string]bool{"cyber": true, "bio": true, "frontier_llm": true, "reasoning_extraction": true, "general_harms": true}

const prefix = "claude refusal"

// Detect reports whether a failed request's failure body is a Claude policy
// refusal.
func Detect(failed bool, body string) (Refusal, bool) {
	if !failed {
		return Refusal{}, false
	}
	text := strings.TrimSpace(body)
	if refusal, ok := fromText(text); ok {
		return refusal, true
	}
	if refusal, ok := fromPayload([]byte(text)); ok {
		return refusal, true
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "triggered restrictions on violative cyber content") && strings.Contains(lower, "anthropic's usage policy") {
		return Refusal{Category: "cyber", Reason: text}, true
	}
	return Refusal{}, false
}

// fromText parses the message CLIProxyAPI builds.
func fromText(text string) (Refusal, bool) {
	if len(text) < len(prefix) || !strings.EqualFold(text[:len(prefix)], prefix) {
		return Refusal{}, false
	}
	rest := text[len(prefix):]
	var refusal Refusal
	switch {
	case rest == "":
		return refusal, true
	case strings.HasPrefix(rest, " ("):
		end := strings.Index(rest, ")")
		if end < 0 {
			return Refusal{}, false
		}
		refusal.Category = strings.ToLower(strings.TrimSpace(rest[2:end]))
		rest = rest[end+1:]
		if rest == "" {
			return refusal, true
		}
		if !strings.HasPrefix(rest, ":") {
			return Refusal{}, false
		}
		refusal.Reason = strings.TrimSpace(rest[1:])
		return refusal, true
	case strings.HasPrefix(rest, ":"):
		refusal.Reason = strings.TrimSpace(rest[1:])
		return refusal, true
	}
	return Refusal{}, false
}

type stopDetails struct {
	Type        string `json:"type"`
	Category    string `json:"category"`
	Explanation string `json:"explanation"`
}

type stopPayload struct {
	StopReason  string          `json:"stop_reason"`
	StopDetails json.RawMessage `json:"stop_details"`
}

// fromPayload reads a Messages response, or a streaming message_delta event,
// whose stop details describe a refusal.
func fromPayload(raw []byte) (Refusal, bool) {
	if !strings.Contains(string(raw), "stop_details") {
		return Refusal{}, false
	}
	var payload struct {
		stopPayload
		Delta stopPayload `json:"delta"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return Refusal{}, false
	}
	for _, candidate := range []stopPayload{payload.Delta, payload.stopPayload} {
		var details stopDetails
		if len(candidate.StopDetails) == 0 || json.Unmarshal(candidate.StopDetails, &details) != nil {
			continue
		}
		category := strings.ToLower(strings.TrimSpace(details.Category))
		if strings.EqualFold(strings.TrimSpace(candidate.StopReason), "refusal") || strings.EqualFold(strings.TrimSpace(details.Type), "refusal") ||
			refusalCategories[category] {
			return Refusal{Category: category, Reason: strings.TrimSpace(details.Explanation)}, true
		}
	}
	return Refusal{}, false
}
