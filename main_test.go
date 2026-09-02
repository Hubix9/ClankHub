package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	store, err := openStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.syncRooms([]RoomConfig{
		{ID: "general", Description: "General", Archived: false},
		{ID: "old", Description: "Archived room", Archived: true},
	}); err != nil {
		t.Fatal(err)
	}
	tmpl, err := templateForTests()
	if err != nil {
		t.Fatal(err)
	}
	return &Server{store: store, tmpl: tmpl, maxMessageLength: defaultMessageLength}
}

func templateForTests() (*template.Template, error) {
	return template.ParseFS(webFiles, "web/index.html", "web/messages.html")
}

func doJSON(t *testing.T, handler http.Handler, method, path, token string, body any, target any) *httptest.ResponseRecorder {
	t.Helper()
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, requestBody)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if target != nil {
		if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
			t.Fatalf("decode response %d: %v; body=%s", response.Code, err, response.Body.String())
		}
	}
	return response
}

func TestAPIFlow(t *testing.T) {
	server := testServer(t)
	handler := server.routes()

	if response := doJSON(t, handler, http.MethodGet, "/healthz", "", nil, nil); response.Code != http.StatusOK {
		t.Fatalf("health status = %d", response.Code)
	}
	if response := doJSON(t, handler, http.MethodGet, "/api/rooms", "", nil, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated rooms status = %d", response.Code)
	}

	const token = "agent-token-for-tests"
	var agent Agent
	if response := doJSON(t, handler, http.MethodPost, "/api/agents/register", "", registerRequest{Name: "alpha", Token: token}, &agent); response.Code != http.StatusCreated {
		t.Fatalf("register status = %d", response.Code)
	}
	if agent.Name != "alpha" || agent.ID == "" {
		t.Fatalf("unexpected agent: %+v", agent)
	}
	if response := doJSON(t, handler, http.MethodPost, "/api/agents/register", "", registerRequest{Name: "alpha", Token: "another-token"}, nil); response.Code != http.StatusConflict {
		t.Fatalf("duplicate name status = %d", response.Code)
	}

	var rooms struct {
		Rooms []Room `json:"rooms"`
	}
	if response := doJSON(t, handler, http.MethodGet, "/api/rooms", token, nil, &rooms); response.Code != http.StatusOK {
		t.Fatalf("rooms status = %d", response.Code)
	}
	if len(rooms.Rooms) != 2 || !rooms.Rooms[1].Archived {
		t.Fatalf("unexpected rooms: %+v", rooms.Rooms)
	}

	var posted Message
	if response := doJSON(t, handler, http.MethodPost, "/api/messages", token, postMessageRequest{RoomID: "general", Body: "hello"}, &posted); response.Code != http.StatusCreated {
		t.Fatalf("post status = %d", response.Code)
	}
	if posted.AgentName != "alpha" || posted.Body != "hello" {
		t.Fatalf("unexpected message: %+v", posted)
	}
	if response := doJSON(t, handler, http.MethodPost, "/api/messages", "wrong-token", postMessageRequest{RoomID: "general", Body: "nope"}, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", response.Code)
	}

	var page MessagePage
	if response := doJSON(t, handler, http.MethodGet, "/api/rooms/general/messages?since=2000-01-01T00%3A00%3A00Z", token, nil, &page); response.Code != http.StatusOK {
		t.Fatalf("read status = %d", response.Code)
	}
	if page.ReadAt == "" || len(page.Messages) != 1 || page.Messages[0].ID != posted.ID || page.NextSince == "" || page.NextAfter == "" {
		t.Fatalf("unexpected page: %+v", page)
	}
	var nextPage MessagePage
	if response := doJSON(t, handler, http.MethodGet, "/api/rooms/general/messages?since="+page.NextSince+"&after_id="+page.NextAfter, token, nil, &nextPage); response.Code != http.StatusOK {
		t.Fatalf("cursor read status = %d", response.Code)
	} else if len(nextPage.Messages) != 0 {
		t.Fatalf("cursor returned messages: %+v", nextPage.Messages)
	}

	if response := doJSON(t, handler, http.MethodPost, "/api/messages", token, postMessageRequest{RoomID: "old", Body: "nope"}, nil); response.Code != http.StatusConflict {
		t.Fatalf("archived post status = %d", response.Code)
	}
	if response := doJSON(t, handler, http.MethodGet, "/api/rooms/general/messages?since=not-a-time", token, nil, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor status = %d", response.Code)
	}
	if response := doJSON(t, handler, http.MethodGet, "/api/rooms/missing/messages", token, nil, nil); response.Code != http.StatusNotFound {
		t.Fatalf("missing room status = %d", response.Code)
	}

	webResponse := httptest.NewRecorder()
	handler.ServeHTTP(webResponse, httptest.NewRequest(http.MethodGet, "/?room=general", nil))
	if webResponse.Code != http.StatusOK || !strings.Contains(webResponse.Body.String(), "ClankHub") || !strings.Contains(webResponse.Body.String(), "hello") {
		t.Fatalf("unexpected web response: status=%d body=%s", webResponse.Code, webResponse.Body.String())
	}
}

func TestMessagesPersistAcrossStoreReopen(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "persistent.db")
	store, err := openStore(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	rooms := []RoomConfig{{ID: "general", Description: "General"}}
	if err := store.syncRooms(rooms); err != nil {
		t.Fatal(err)
	}
	agent, err := store.registerAgent("persistent-agent", "persistent-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.addMessage(agent, "general", "persist me"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStore(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.syncRooms(rooms); err != nil {
		t.Fatal(err)
	}
	restoredAgent, err := reopened.authenticate("persistent-token")
	if err != nil {
		t.Fatal(err)
	}
	messages, _, err := reopened.listMessages("general", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Body != "persist me" || messages[0].AgentID != restoredAgent.ID {
		t.Fatalf("unexpected persisted messages: %+v", messages)
	}
}

func TestLoadConfigMessageLength(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte("listen: 127.0.0.1:8080\ndatabase: messages.db\nmax_message_length: 42\nrooms:\n  - id: general\n    description: General\n")
	if err := os.WriteFile(configPath, contents, 0600); err != nil {
		t.Fatal(err)
	}

	config, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxMessageLength != 42 {
		t.Fatalf("max message length = %d, want 42", config.MaxMessageLength)
	}
}

func TestConfigurableMessageLength(t *testing.T) {
	server := testServer(t)
	server.maxMessageLength = 5
	server.store.maxMessageLength = 5
	handler := server.routes()

	const token = "limited-agent-token"
	if response := doJSON(t, handler, http.MethodPost, "/api/agents/register", "", registerRequest{Name: "limited-agent", Token: token}, nil); response.Code != http.StatusCreated {
		t.Fatalf("register status = %d", response.Code)
	}
	if response := doJSON(t, handler, http.MethodPost, "/api/messages", token, postMessageRequest{RoomID: "general", Body: "123456"}, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("oversized message status = %d", response.Code)
	}
}

func TestMessageWindowsSearchAndPermalinks(t *testing.T) {
	server := testServer(t)
	agent, err := server.store.registerAgent("window-agent", "window-token")
	if err != nil {
		t.Fatal(err)
	}
	for number := 1; number <= 204; number++ {
		id := fmt.Sprintf("window-%03d", number)
		createdAt := fmt.Sprintf("2026-01-01T00:%02d:%02dZ", number/60, number%60)
		body := fmt.Sprintf("message %d", number)
		if number == 3 {
			body += " needle"
		}
		if _, err := server.store.db.Exec(`
INSERT INTO messages (id, room_id, agent_id, body, created_at) VALUES (?, ?, ?, ?, ?)
`, id, "general", agent.ID, body, createdAt); err != nil {
			t.Fatal(err)
		}
	}

	latest, err := server.store.latestMessagePage("general", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !latest.HasMore || len(latest.Messages) != 2 || latest.Messages[0].Body != "message 203" || latest.Messages[1].Body != "message 204" {
		t.Fatalf("unexpected latest window: %+v", latest)
	}

	older, err := server.store.messagePageBefore("general", latest.NextBefore, latest.NextBeforeID, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !older.HasMore || len(older.Messages) != 2 || older.Messages[0].Body != "message 201" || older.Messages[1].Body != "message 202" {
		t.Fatalf("unexpected older window: %+v", older)
	}

	around, err := server.store.messagePageAround("general", "window-003", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(around.Messages) != 4 || around.Messages[2].ID != "window-003" || around.Messages[3].ID != "window-004" {
		t.Fatalf("unexpected nearby window: %+v", around)
	}

	searchResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(searchResponse, httptest.NewRequest(http.MethodGet, "/ui/rooms/general/messages/search?q=needle", nil))
	searchBody := searchResponse.Body.String()
	if searchResponse.Code != http.StatusOK || !strings.Contains(searchBody, "message 3 needle") || !strings.Contains(searchBody, "#message-window-003") {
		t.Fatalf("unexpected search response: status=%d body=%s", searchResponse.Code, searchBody)
	}

	aroundResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(aroundResponse, httptest.NewRequest(http.MethodGet, "/ui/rooms/general/messages?mode=around&message_id=window-003", nil))
	if aroundResponse.Code != http.StatusOK || !strings.Contains(aroundResponse.Body.String(), `id="message-window-003"`) {
		t.Fatalf("unexpected permalink response: status=%d body=%s", aroundResponse.Code, aroundResponse.Body.String())
	}

	indexResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(indexResponse, httptest.NewRequest(http.MethodGet, "/?room=general", nil))
	if indexResponse.Code != http.StatusOK || !strings.Contains(indexResponse.Body.String(), "message 204") {
		t.Fatalf("index did not start at latest messages: status=%d", indexResponse.Code)
	}

	initialWindowResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(initialWindowResponse, httptest.NewRequest(http.MethodGet, "/ui/rooms/general/messages", nil))
	initialWindow := initialWindowResponse.Body.String()
	if initialWindowResponse.Code != http.StatusOK ||
		!strings.Contains(initialWindow, `data-has-more="true"`) ||
		!strings.Contains(initialWindow, `id="message-window-005"`) ||
		!strings.Contains(initialWindow, `id="message-window-204"`) ||
		strings.Contains(initialWindow, `id="message-window-001"`) {
		t.Fatalf("default web window did not contain exactly the newest history: status=%d", initialWindowResponse.Code)
	}

	olderWindowResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(olderWindowResponse, httptest.NewRequest(http.MethodGet, "/ui/rooms/general/messages?mode=older&before=2026-01-01T00%3A00%3A05Z&before_id=window-005", nil))
	olderWindow := olderWindowResponse.Body.String()
	if olderWindowResponse.Code != http.StatusOK ||
		!strings.Contains(olderWindow, `id="message-window-001"`) ||
		!strings.Contains(olderWindow, `id="message-window-004"`) ||
		strings.Contains(olderWindow, `id="message-window-005"`) {
		t.Fatalf("older web window did not contain the messages beyond the default limit: status=%d", olderWindowResponse.Code)
	}

	var backward MessagePage
	backwardResponse := doJSON(t, server.routes(), http.MethodGet, "/api/rooms/general/messages?before=2026-01-01T00%3A00%3A03Z&before_id=window-003&limit=2", "window-token", nil, &backward)
	if backwardResponse.Code != http.StatusOK || len(backward.Messages) != 2 || backward.Messages[0].ID != "window-001" || backward.Messages[1].ID != "window-002" {
		t.Fatalf("unexpected API backward page: status=%d page=%+v", backwardResponse.Code, backward)
	}
}
