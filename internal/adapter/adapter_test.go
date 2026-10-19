package adapter

import (
	"strings"
	"testing"
)

func TestFrameBodyRetryNote(t *testing.T) {
	normal := FrameBody(SendRequest{MessageID: "m_1", ConversationID: "c_1", SenderLabel: "operator", Body: "hello"})
	if strings.Contains(normal, "retry of") {
		t.Fatalf("normal frame mentions a retry: %q", normal)
	}
	retry := FrameBody(SendRequest{
		MessageID: "m_2", ConversationID: "c_1", SenderLabel: "operator",
		Body: "hello", RetryOf: "m_1",
	})
	for _, want := range []string{"retry of m_1", "duplicate native submission is possible"} {
		if !strings.Contains(retry, want) {
			t.Fatalf("retry frame %q lacks %q", retry, want)
		}
	}
}
