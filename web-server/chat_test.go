package main

import (
	"testing"
	"time"
)

func TestChatHistoryLimit(t *testing.T) {
	chatRoomsMu.Lock()
	chatHistory = make(map[string][]ChatMessage)
	chatRoomsMu.Unlock()

	clinic := "TestClinic"
	for i := 0; i < chatHistoryLimit+5; i++ {
		appendChatHistory(clinic, ChatMessage{
			Type:       "chat",
			ClinicName: clinic,
			Sender:     "Nurse",
			Role:       "nurse",
			Text:       "msg",
			Timestamp:  time.Now().UTC().Format(time.RFC3339),
		})
	}

	history := chatHistoryFor(clinic)
	if len(history) != chatHistoryLimit {
		t.Fatalf("expected %d history messages, got %d", chatHistoryLimit, len(history))
	}
}

func TestStringsTrim(t *testing.T) {
	if got := stringsTrim("  Dr. Smith  ", "fallback"); got != "Dr. Smith" {
		t.Fatalf("unexpected trim result: %q", got)
	}
	if got := stringsTrim("   ", "fallback"); got != "fallback" {
		t.Fatalf("expected fallback, got %q", got)
	}
}

func TestValidateChatImage(t *testing.T) {
	tinyPNG := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	if err := validateChatImage(tinyPNG, "image/png"); err != nil {
		t.Fatalf("expected valid png, got %v", err)
	}
	if err := validateChatImage(tinyPNG, "image/bmp"); err == nil {
		t.Fatal("expected unsupported mime to fail")
	}
	if err := validateChatImage("not-base64!!!", "image/png"); err == nil {
		t.Fatal("expected invalid base64 to fail")
	}
}
