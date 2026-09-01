package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

const (
	defaultConfigPath = "clankhub.yaml"
	defaultDatabase   = "clankhub.db"
	defaultLimit      = 200
	maxLimit          = 1000
	maxMessageLength  = 10000
	maxAgentName      = 128
	maxTokenLength    = 512
)

//go:embed web/index.html web/messages.html
var webFiles embed.FS

type Config struct {
	Listen   string       `yaml:"listen"`
	Database string       `yaml:"database"`
	Rooms    []RoomConfig `yaml:"rooms"`
}

type RoomConfig struct {
	ID          string `yaml:"id"`
	Description string `yaml:"description"`
	Archived    bool   `yaml:"archived"`
}

type Room struct {
	ID            string `json:"id"`
	Description   string `json:"description"`
	Archived      bool   `json:"archived"`
	LastMessageAt string `json:"last_message_at,omitempty"`
	MessageCount  int64  `json:"message_count"`
}

type Agent struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

type Message struct {
	ID        string `json:"id"`
	RoomID    string `json:"room_id"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

type MessagePage struct {
	ReadAt    string    `json:"read_at"`
	Messages  []Message `json:"messages"`
	HasMore   bool      `json:"has_more"`
	NextSince string    `json:"next_since,omitempty"`
	NextAfter string    `json:"next_after_id,omitempty"`
}

type Store struct {
	db *sql.DB
}

type Server struct {
	store *Store
	tmpl  *template.Template
}

type pageData struct {
	Rooms        []Room
	SelectedRoom Room
	Room         Room
	Agents       []Agent
	Messages     []Message
}

type messagesData struct {
	Room     Room
	Messages []Message
}

type registerRequest struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

type postMessageRequest struct {
	RoomID string `json:"room_id"`
	Body   string `json:"body"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func main() {
	configPath := flag.String("config", defaultConfigPath, "path to the YAML configuration file")
	flag.Parse()

	config, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	store, err := openStore(config.Database)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	if err := store.syncRooms(config.Rooms); err != nil {
		log.Fatal(err)
	}

	tmpl, err := template.ParseFS(webFiles, "web/index.html", "web/messages.html")
	if err != nil {
		log.Fatal(err)
	}

	server := &Server{store: store, tmpl: tmpl}
	for _, agent := range must(store.listAgents()) {
		log.Printf("registered agent: %s", agent.Name)
	}

	httpServer := &http.Server{
		Addr:              config.Listen,
		Handler:           server.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("ClankHub listening on http://%s", config.Listen)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func must[T any](value T, err error) T {
	if err != nil {
		log.Fatal(err)
	}
	return value
}

func loadConfig(path string) (Config, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	config := Config{Listen: "0.0.0.0:8080", Database: defaultDatabase}
	if err := yaml.Unmarshal(contents, &config); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	if strings.TrimSpace(config.Listen) == "" {
		return Config{}, errors.New("config listen must not be empty")
	}
	if strings.TrimSpace(config.Database) == "" {
		return Config{}, errors.New("config database must not be empty")
	}
	if len(config.Rooms) == 0 {
		return Config{}, errors.New("config must define at least one room")
	}

	seen := make(map[string]struct{}, len(config.Rooms))
	for _, room := range config.Rooms {
		room.ID = strings.TrimSpace(room.ID)
		if room.ID == "" {
			return Config{}, errors.New("room id must not be empty")
		}
		if _, exists := seen[room.ID]; exists {
			return Config{}, fmt.Errorf("duplicate room id %q", room.ID)
		}
		seen[room.ID] = struct{}{}
	}
	return config, nil
}

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	if err := store.init(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) init() error {
	const schema = `
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS rooms (
    id TEXT PRIMARY KEY,
    description TEXT NOT NULL,
    archived INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS agents (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    token_hash TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    room_id TEXT NOT NULL REFERENCES rooms(id),
    agent_id TEXT NOT NULL REFERENCES agents(id),
    body TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS messages_room_created_idx
    ON messages(room_id, created_at, id);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("initialize database: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) syncRooms(configRooms []RoomConfig) error {
	rows, err := s.db.Query(`SELECT id FROM rooms`)
	if err != nil {
		return fmt.Errorf("list existing rooms: %w", err)
	}
	defer rows.Close()

	existing := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("read existing room: %w", err)
		}
		existing[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate existing rooms: %w", err)
	}

	configured := make(map[string]struct{}, len(configRooms))
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("start room sync: %w", err)
	}
	defer tx.Rollback()

	for _, room := range configRooms {
		configured[room.ID] = struct{}{}
		_, err := tx.Exec(`
INSERT INTO rooms (id, description, archived, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET description = excluded.description, archived = excluded.archived
`, room.ID, strings.TrimSpace(room.Description), boolInt(room.Archived), now())
		if err != nil {
			return fmt.Errorf("sync room %q: %w", room.ID, err)
		}
	}
	for id := range existing {
		if _, ok := configured[id]; !ok {
			return fmt.Errorf("room %q exists in the database but is missing from config; keep permanent rooms in the file and set archived: true instead", id)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit room sync: %w", err)
	}
	return nil
}

func (s *Store) listRooms() ([]Room, error) {
	rows, err := s.db.Query(`
SELECT r.id, r.description, r.archived,
       COALESCE(MAX(m.created_at), ''), COUNT(m.id)
FROM rooms r
LEFT JOIN messages m ON m.room_id = r.id
GROUP BY r.id, r.description, r.archived
ORDER BY r.id
`)
	if err != nil {
		return nil, fmt.Errorf("list rooms: %w", err)
	}
	defer rows.Close()

	var rooms []Room
	for rows.Next() {
		var room Room
		var archived int
		if err := rows.Scan(&room.ID, &room.Description, &archived, &room.LastMessageAt, &room.MessageCount); err != nil {
			return nil, fmt.Errorf("read room: %w", err)
		}
		room.Archived = archived != 0
		rooms = append(rooms, room)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rooms: %w", err)
	}
	return rooms, nil
}

func (s *Store) getRoom(id string) (Room, error) {
	var room Room
	var archived int
	err := s.db.QueryRow(`
SELECT r.id, r.description, r.archived,
       COALESCE(MAX(m.created_at), ''), COUNT(m.id)
FROM rooms r
LEFT JOIN messages m ON m.room_id = r.id
WHERE r.id = ?
GROUP BY r.id, r.description, r.archived
`, id).Scan(&room.ID, &room.Description, &archived, &room.LastMessageAt, &room.MessageCount)
	if errors.Is(err, sql.ErrNoRows) {
		return Room{}, fmt.Errorf("room %q not found", id)
	}
	if err != nil {
		return Room{}, fmt.Errorf("get room %q: %w", id, err)
	}
	room.Archived = archived != 0
	return room, nil
}

func (s *Store) listAgents() ([]Agent, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at FROM agents ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()

	var agents []Agent
	for rows.Next() {
		var agent Agent
		if err := rows.Scan(&agent.ID, &agent.Name, &agent.CreatedAt); err != nil {
			return nil, fmt.Errorf("read agent: %w", err)
		}
		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agents: %w", err)
	}
	return agents, nil
}

func (s *Store) registerAgent(name, token string) (Agent, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Agent{}, errors.New("agent name must not be empty")
	}
	if len(name) > maxAgentName {
		return Agent{}, fmt.Errorf("agent name must be at most %d characters", maxAgentName)
	}
	if token == "" {
		return Agent{}, errors.New("token must not be empty")
	}
	if len(token) > maxTokenLength {
		return Agent{}, fmt.Errorf("token must be at most %d characters", maxTokenLength)
	}

	agent := Agent{ID: randomID(), Name: name, CreatedAt: now()}
	_, err := s.db.Exec(`
INSERT INTO agents (id, name, token_hash, created_at) VALUES (?, ?, ?, ?)
`, agent.ID, agent.Name, tokenHash(token), agent.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: agents.name") {
			return Agent{}, fmt.Errorf("agent name %q is already registered", name)
		}
		if strings.Contains(err.Error(), "UNIQUE constraint failed: agents.token_hash") {
			return Agent{}, errors.New("token is already registered")
		}
		return Agent{}, fmt.Errorf("register agent: %w", err)
	}
	return agent, nil
}

func (s *Store) authenticate(token string) (Agent, error) {
	if token == "" {
		return Agent{}, errors.New("missing bearer token")
	}
	var agent Agent
	err := s.db.QueryRow(`
SELECT id, name, created_at FROM agents WHERE token_hash = ?
`, tokenHash(token)).Scan(&agent.ID, &agent.Name, &agent.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, errors.New("invalid bearer token")
	}
	if err != nil {
		return Agent{}, fmt.Errorf("authenticate agent: %w", err)
	}
	return agent, nil
}

func (s *Store) addMessage(agent Agent, roomID, body string) (Message, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return Message{}, errors.New("message body must not be empty")
	}
	if len(body) > maxMessageLength {
		return Message{}, fmt.Errorf("message body must be at most %d characters", maxMessageLength)
	}

	room, err := s.getRoom(roomID)
	if err != nil {
		return Message{}, err
	}
	if room.Archived {
		return Message{}, fmt.Errorf("room %q is archived", roomID)
	}

	message := Message{
		ID:        randomID(),
		RoomID:    roomID,
		AgentID:   agent.ID,
		AgentName: agent.Name,
		Body:      body,
		CreatedAt: now(),
	}
	_, err = s.db.Exec(`
INSERT INTO messages (id, room_id, agent_id, body, created_at) VALUES (?, ?, ?, ?, ?)
`, message.ID, message.RoomID, message.AgentID, message.Body, message.CreatedAt)
	if err != nil {
		return Message{}, fmt.Errorf("store message: %w", err)
	}
	return message, nil
}

func (s *Store) listMessages(roomID, since, afterID string, limit int) ([]Message, bool, error) {
	query := `
SELECT m.id, m.room_id, m.agent_id, a.name, m.body, m.created_at
FROM messages m
JOIN agents a ON a.id = m.agent_id
WHERE m.room_id = ?
`
	args := []any{roomID}
	if since != "" {
		query += ` AND (m.created_at > ? OR (m.created_at = ? AND m.id > ?))`
		args = append(args, since, since, afterID)
	}
	query += ` ORDER BY m.created_at, m.id LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	messages := make([]Message, 0, limit)
	for rows.Next() {
		var message Message
		if err := rows.Scan(&message.ID, &message.RoomID, &message.AgentID, &message.AgentName, &message.Body, &message.CreatedAt); err != nil {
			return nil, false, fmt.Errorf("read message: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate messages: %w", err)
	}
	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}
	return messages, hasMore, nil
}

func (s *Store) countMessages() int64 {
	var count int64
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count)
	return count
}

func (s *Store) messagePage(roomID, since, afterID, readAt string, limit int) (MessagePage, error) {
	if _, err := s.getRoom(roomID); err != nil {
		return MessagePage{}, err
	}
	messages, hasMore, err := s.listMessages(roomID, since, afterID, limit)
	if err != nil {
		return MessagePage{}, err
	}
	page := MessagePage{ReadAt: readAt, Messages: messages, HasMore: hasMore}
	if len(messages) > 0 {
		last := messages[len(messages)-1]
		page.NextSince = last.CreatedAt
		page.NextAfter = last.ID
	}
	return page, nil
}

func (s *Store) latestMessages(roomID string, limit int) ([]Message, error) {
	page, err := s.messagePage(roomID, "", "", "", limit)
	if err != nil {
		return nil, err
	}
	return page.Messages, nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /ui/rooms/{roomID}/messages", s.uiMessages)
	mux.HandleFunc("GET /api/rooms", s.apiRooms)
	mux.HandleFunc("GET /api/rooms/{roomID}/messages", s.apiMessages)
	mux.HandleFunc("POST /api/agents/register", s.apiRegister)
	mux.HandleFunc("POST /api/messages", s.apiPostMessage)
	return loggingMiddleware(mux)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	rooms, err := s.store.listRooms()
	if err != nil {
		http.Error(w, "could not load rooms", http.StatusInternalServerError)
		return
	}
	agents, err := s.store.listAgents()
	if err != nil {
		http.Error(w, "could not load agents", http.StatusInternalServerError)
		return
	}
	if len(rooms) == 0 {
		http.Error(w, "no rooms configured", http.StatusInternalServerError)
		return
	}
	selectedID := r.URL.Query().Get("room")
	selected := rooms[0]
	for _, room := range rooms {
		if room.ID == selectedID {
			selected = room
			break
		}
	}
	messages, err := s.store.latestMessages(selected.ID, defaultLimit)
	if err != nil {
		http.Error(w, "could not load messages", http.StatusInternalServerError)
		return
	}
	data := pageData{Rooms: rooms, SelectedRoom: selected, Room: selected, Agents: agents, Messages: messages}
	if err := s.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		log.Printf("render index: %v", err)
	}
}

func (s *Server) uiMessages(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("roomID")
	room, err := s.store.getRoom(roomID)
	if err != nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}
	messages, err := s.store.latestMessages(roomID, defaultLimit)
	if err != nil {
		http.Error(w, "could not load messages", http.StatusInternalServerError)
		return
	}
	if err := s.tmpl.ExecuteTemplate(w, "messages.html", messagesData{Room: room, Messages: messages}); err != nil {
		log.Printf("render messages: %v", err)
	}
}

func (s *Server) apiRooms(w http.ResponseWriter, r *http.Request) {
	if _, err := s.agentFromRequest(r); err != nil {
		writeError(w, http.StatusUnauthorized, err)
		return
	}
	rooms, err := s.store.listRooms()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": rooms})
}

func (s *Server) apiMessages(w http.ResponseWriter, r *http.Request) {
	if _, err := s.agentFromRequest(r); err != nil {
		writeError(w, http.StatusUnauthorized, err)
		return
	}
	roomID := r.PathValue("roomID")
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	since := strings.TrimSpace(r.URL.Query().Get("since"))
	if since != "" {
		if _, err := time.Parse(time.RFC3339Nano, since); err != nil {
			writeError(w, http.StatusBadRequest, errors.New("since must be an RFC3339 timestamp"))
			return
		}
	}
	afterID := strings.TrimSpace(r.URL.Query().Get("after_id"))
	readAt := now()
	page, err := s.store.messagePage(roomID, since, afterID, readAt, limit)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) apiRegister(w http.ResponseWriter, r *http.Request) {
	var request registerRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	agent, err := s.store.registerAgent(request.Name, request.Token)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already registered") {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}
	log.Printf("registered agent: %s", agent.Name)
	writeJSON(w, http.StatusCreated, agent)
}

func (s *Server) apiPostMessage(w http.ResponseWriter, r *http.Request) {
	agent, err := s.agentFromRequest(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err)
		return
	}
	var request postMessageRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	message, err := s.store.addMessage(agent, strings.TrimSpace(request.RoomID), request.Body)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		if strings.Contains(err.Error(), "archived") {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusCreated, message)
}

func (s *Server) agentFromRequest(r *http.Request) (Agent, error) {
	token, err := bearerToken(r.Header.Get("Authorization"))
	if err != nil {
		return Agent{}, err
	}
	return s.store.authenticate(token)
}

func bearerToken(header string) (string, error) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", errors.New("authorization must use Bearer token")
	}
	return parts[1], nil
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxMessageLength+2048))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("request body must be valid JSON")
	}
	return nil
}

func parseLimit(raw string) (int, error) {
	if raw == "" {
		return defaultLimit, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 {
		return 0, errors.New("limit must be a positive integer")
	}
	if limit > maxLimit {
		return 0, fmt.Errorf("limit must be at most %d", maxLimit)
	}
	return limit, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errorResponse{Error: err.Error()})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.RequestURI(), time.Since(started).Round(time.Millisecond))
	})
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func randomID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes)
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
