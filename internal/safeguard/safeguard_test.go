package safeguard

import "testing"

func TestDetectRecognizesCLIProxyAPIRefusalMessages(t *testing.T) {
	for _, test := range []struct {
		body     string
		category string
		reason   string
	}{
		// The non-streaming and streaming executors both publish the failure
		// text from clienterror.ClaudePolicyRefusalMessage.
		{"claude refusal (cyber): This request may facilitate unauthorized access.", "cyber", "This request may facilitate unauthorized access."},
		{"claude refusal: The assistant declined.", "", "The assistant declined."},
		{"claude refusal (bio)", "bio", ""},
		{"claude refusal", "", ""},
		{`claude refusal: {"type":"refusal"}`, "", `{"type":"refusal"}`},
		{`{"type":"message","stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber","explanation":"blocked"}}`, "cyber", "blocked"},
		{`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_details":{"category":"general_harms"}}}`, "general_harms", ""},
		{"Your request triggered restrictions on violative cyber content per Anthropic's Usage Policy.", "cyber",
			"Your request triggered restrictions on violative cyber content per Anthropic's Usage Policy."},
	} {
		refusal, ok := Detect(true, test.body)
		if !ok || refusal.Category != test.category || refusal.Reason != test.reason {
			t.Errorf("Detect(%q) = %+v, %v", test.body, refusal, ok)
		}
	}
}

func TestDetectIgnoresOtherFailures(t *testing.T) {
	for _, body := range []string{
		"",
		"upstream stream closed before message_stop",
		`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		`{"stop_reason":"end_turn","stop_details":{"type":"other"}}`,
		"claude refusals are documented elsewhere",
		"claude refusal (cyber",
	} {
		if refusal, ok := Detect(true, body); ok {
			t.Errorf("Detect(%q) = %+v", body, refusal)
		}
	}
	if _, ok := Detect(false, "claude refusal (cyber): x"); ok {
		t.Fatal("a successful request is not a refusal")
	}
}
