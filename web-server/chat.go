package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const chatHistoryLimit = 100
const maxChatImageBytes = 2 * 1024 * 1024

var allowedChatImageMimes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

type ChatMessage struct {
	Type       string `json:"type"`
	ClinicName string `json:"clinic_name"`
	Sender     string `json:"sender"`
	Role       string `json:"role"`
	Text       string `json:"text"`
	Image      string `json:"image,omitempty"`
	ImageMime  string `json:"image_mime,omitempty"`
	Timestamp  string `json:"timestamp"`
}

type chatClient struct {
	conn   *websocket.Conn
	clinic string
	sender string
	role   string
}

var (
	chatRoomsMu sync.Mutex
	chatRooms   = make(map[string]map[*chatClient]bool)
	chatHistory = make(map[string][]ChatMessage)
)

func appendChatHistory(clinic string, msg ChatMessage) {
	chatRoomsMu.Lock()
	defer chatRoomsMu.Unlock()

	history := append(chatHistory[clinic], msg)
	if len(history) > chatHistoryLimit {
		history = history[len(history)-chatHistoryLimit:]
	}
	chatHistory[clinic] = history
}

func chatHistoryFor(clinic string) []ChatMessage {
	chatRoomsMu.Lock()
	defer chatRoomsMu.Unlock()

	history := chatHistory[clinic]
	if len(history) == 0 {
		return nil
	}
	out := make([]ChatMessage, len(history))
	copy(out, history)
	return out
}

func addChatClient(client *chatClient) {
	chatRoomsMu.Lock()
	defer chatRoomsMu.Unlock()

	if chatRooms[client.clinic] == nil {
		chatRooms[client.clinic] = make(map[*chatClient]bool)
	}
	chatRooms[client.clinic][client] = true
}

func removeChatClient(client *chatClient) {
	chatRoomsMu.Lock()
	defer chatRoomsMu.Unlock()

	if room := chatRooms[client.clinic]; room != nil {
		delete(room, client)
		if len(room) == 0 {
			delete(chatRooms, client.clinic)
		}
	}
}

func broadcastChat(clinic string, msg ChatMessage, skip *chatClient) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return
	}

	chatRoomsMu.Lock()
	room := chatRooms[clinic]
	clients := make([]*chatClient, 0, len(room))
	for client := range room {
		if skip != nil && client == skip {
			continue
		}
		clients = append(clients, client)
	}
	chatRoomsMu.Unlock()

	for _, client := range clients {
		if err := client.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			log.Printf("Chat WS write error for clinic %s: %v", clinic, err)
			client.conn.Close()
			removeChatClient(client)
		}
	}
}

func sendChatHistory(client *chatClient) {
	for _, msg := range chatHistoryFor(client.clinic) {
		payload, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		if err := client.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			log.Printf("Chat history write error for clinic %s: %v", client.clinic, err)
			return
		}
	}
}

func handleChatWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Chat WS upgrade error: %v", err)
		return
	}

	client := &chatClient{conn: conn}
	log.Printf("Chat WS connected")

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var envelope ChatMessage
		if err := json.Unmarshal(msg, &envelope); err != nil {
			continue
		}

		switch envelope.Type {
		case "register":
			if envelope.ClinicName == "" {
				continue
			}
			if client.clinic != "" && client.clinic != envelope.ClinicName {
				removeChatClient(client)
			}
			client.clinic = safe(envelope.ClinicName)
			client.sender = stringsTrim(envelope.Sender, "Unknown")
			client.role = stringsTrim(envelope.Role, "user")
			addChatClient(client)
			sendChatHistory(client)
			log.Printf("Chat WS registered: clinic=%s sender=%s role=%s", client.clinic, client.sender, client.role)
		case "chat":
			if client.clinic == "" || (strings.TrimSpace(envelope.Text) == "" && envelope.Image == "") {
				continue
			}
			if err := validateChatImage(envelope.Image, envelope.ImageMime); err != nil {
				log.Printf("Chat image rejected for clinic %s: %v", client.clinic, err)
				continue
			}
			chatMsg := ChatMessage{
				Type:       "chat",
				ClinicName: client.clinic,
				Sender:     client.sender,
				Role:       client.role,
				Text:       envelope.Text,
				Image:      envelope.Image,
				ImageMime:  envelope.ImageMime,
				Timestamp:  time.Now().UTC().Format(time.RFC3339),
			}
			appendChatHistory(client.clinic, chatMsg)
			broadcastChat(client.clinic, chatMsg, nil)
		}
	}

	if client.clinic != "" {
		removeChatClient(client)
		log.Printf("Chat WS disconnected: clinic=%s sender=%s", client.clinic, client.sender)
	}
	conn.Close()
}

func stringsTrim(s, fallback string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	return s
}

func validateChatImage(imageB64, mime string) error {
	if imageB64 == "" {
		return nil
	}
	mime = strings.TrimSpace(strings.ToLower(mime))
	if !allowedChatImageMimes[mime] {
		return fmt.Errorf("unsupported image type %q", mime)
	}
	data, err := base64.StdEncoding.DecodeString(imageB64)
	if err != nil {
		return fmt.Errorf("invalid base64 image: %w", err)
	}
	if len(data) > maxChatImageBytes {
		return fmt.Errorf("image exceeds %d bytes", maxChatImageBytes)
	}
	return nil
}
