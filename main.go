package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"embed"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"
)

//go:embed templates/* static/*
var embeddedFiles embed.FS

// Constants
const (
	chunkSize     = 4 * 1024 * 1024 // 4MB
	sessionExpiry = 24 * time.Hour
	roomExpiry    = 30 * time.Minute
	sweepInterval = 5 * time.Minute
)

// Room status
const (
	StatusPending  = "pending"
	StatusActive   = "active"
	StatusComplete = "complete"
)

// Types
type Room struct {
	Token        string
	OwnerEmail   string
	SenderConn   *websocket.Conn
	ReceiverConn *websocket.Conn
	SenderMu     sync.Mutex
	ReceiverMu   sync.Mutex
	Created      time.Time
	Status       string
}

type User struct {
	Email        string `json:"email"`
	PasswordHash string `json:"password_hash"`
}

type Session struct {
	Email   string
	Created time.Time
}

// Global state
var (
	rooms           sync.Map
	sessions        sync.Map
	activeTransfers atomic.Int32
	maxConcurrent   int
	users           []User
	usersMu         sync.RWMutex

	// Config from env
	port          string
	adminEmail    string
	adminPassword string
	publicBaseURL string
	dataDir       string
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
	ReadBufferSize:  1024 * 1024,
	WriteBufferSize: 1024 * 1024,
}

// ── User storage ─────────────────────────────────────────────────────────────

func usersFilePath() string {
	return filepath.Join(dataDir, "users.json")
}

func loadUsers() {
	usersMu.Lock()
	defer usersMu.Unlock()
	data, err := os.ReadFile(usersFilePath())
	if os.IsNotExist(err) {
		users = []User{}
		return
	}
	if err != nil {
		log.Printf("loadUsers: %v", err)
		users = []User{}
		return
	}
	if err := json.Unmarshal(data, &users); err != nil {
		log.Printf("loadUsers unmarshal: %v", err)
		users = []User{}
	}
}

func saveUsers() error {
	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	tmp := usersFilePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, usersFilePath())
}

func findUser(email string) *User {
	usersMu.RLock()
	defer usersMu.RUnlock()
	for i := range users {
		if strings.EqualFold(users[i].Email, email) {
			return &users[i]
		}
	}
	return nil
}

// ── Session helpers ───────────────────────────────────────────────────────────

func createSession(email string) string {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	sessions.Store(token, &Session{Email: email, Created: time.Now()})
	return token
}

func getSession(r *http.Request) *Session {
	c, err := r.Cookie("session")
	if err != nil {
		return nil
	}
	val, ok := sessions.Load(c.Value)
	if !ok {
		return nil
	}
	s := val.(*Session)
	if time.Since(s.Created) > sessionExpiry {
		sessions.Delete(c.Value)
		return nil
	}
	return s
}

func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if getSession(r) == nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

func requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("admin_session")
		if err != nil {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		val, ok := sessions.Load(c.Value)
		if !ok {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		s := val.(*Session)
		if s.Email != "__admin__" || time.Since(s.Created) > sessionExpiry {
			sessions.Delete(c.Value)
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

// ── Room helpers ──────────────────────────────────────────────────────────────

func createRoom(ownerEmail string) *Room {
	b := make([]byte, 16)
	rand.Read(b)
	token := hex.EncodeToString(b)
	room := &Room{
		Token:      token,
		OwnerEmail: ownerEmail,
		Created:    time.Now(),
		Status:     StatusPending,
	}
	rooms.Store(token, room)
	return room
}

func getRoom(token string) *Room {
	val, ok := rooms.Load(token)
	if !ok {
		return nil
	}
	return val.(*Room)
}

func deleteRoom(token string) {
	rooms.Delete(token)
}

func roomSweeper() {
	ticker := time.NewTicker(sweepInterval)
	for range ticker.C {
		now := time.Now()
		rooms.Range(func(key, val any) bool {
			room := val.(*Room)
			if room.Status == StatusPending && now.Sub(room.Created) > roomExpiry {
				room.SenderMu.Lock()
				if room.SenderConn != nil {
					room.SenderConn.WriteJSON(map[string]string{"type": "error", "message": "room expired"})
					room.SenderConn.Close()
				}
				room.SenderMu.Unlock()
				rooms.Delete(key)
			}
			return true
		})
	}
}

// ── URL helper ────────────────────────────────────────────────────────────────

func baseURL(r *http.Request) string {
	if publicBaseURL != "" {
		return strings.TrimRight(publicBaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ── HTTP handlers ─────────────────────────────────────────────────────────────

func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

func handleLoginGet(w http.ResponseWriter, r *http.Request) {
	tmpl, err := embeddedFiles.ReadFile("templates/login.html")
	if err != nil {
		http.Error(w, "template error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Write(tmpl)
}

func handleLoginPost(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")

	u := findUser(email)
	if u == nil {
		renderLoginError(w, "Invalid credentials")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		renderLoginError(w, "Invalid credentials")
		return
	}
	token := createSession(email)
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

func renderLoginError(w http.ResponseWriter, msg string) {
	tmpl, _ := embeddedFiles.ReadFile("templates/login.html")
	body := strings.ReplaceAll(string(tmpl), "{{.Error}}", `<p class="error">`+msg+`</p>`)
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(body))
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("session")
	if err == nil {
		sessions.Delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Value: "", MaxAge: -1, Path: "/"})
	http.Redirect(w, r, "/login", http.StatusFound)
}

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	tmpl, err := embeddedFiles.ReadFile("templates/dashboard.html")
	if err != nil {
		http.Error(w, "template error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Write(tmpl)
}

func handleCreateLink(w http.ResponseWriter, r *http.Request) {
	s := getSession(r)
	if s == nil {
		http.Error(w, "unauthorized", 401)
		return
	}

	if maxConcurrent > 0 && int(activeTransfers.Load()) >= maxConcurrent {
		http.Error(w, "server at capacity", http.StatusServiceUnavailable)
		return
	}

	room := createRoom(s.Email)
	url := baseURL(r) + "/transfer/" + room.Token
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": url, "token": room.Token})
}

func handleTransfer(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		http.NotFound(w, r)
		return
	}
	room := getRoom(token)
	if room == nil {
		http.Error(w, "transfer not found", 404)
		return
	}
	tmpl, err := embeddedFiles.ReadFile("templates/transfer.html")
	if err != nil {
		http.Error(w, "template error", 500)
		return
	}
	body := strings.ReplaceAll(string(tmpl), "{{.Token}}", token)
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(body))
}

// ── Admin handlers ────────────────────────────────────────────────────────────

func handleAdminLoginGet(w http.ResponseWriter, r *http.Request) {
	tmpl, err := embeddedFiles.ReadFile("templates/admin_login.html")
	if err != nil {
		http.Error(w, "template error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Write(tmpl)
}

func handleAdminLoginPost(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")

	if !strings.EqualFold(email, adminEmail) || password != adminPassword {
		tmpl, _ := embeddedFiles.ReadFile("templates/admin_login.html")
		body := strings.ReplaceAll(string(tmpl), "{{.Error}}", `<p class="error">Invalid credentials</p>`)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(body))
		return
	}
	token := createSession("__admin__")
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/admin", http.StatusFound)
}

func handleAdmin(w http.ResponseWriter, r *http.Request) {
	usersMu.RLock()
	userList := make([]User, len(users))
	copy(userList, users)
	usersMu.RUnlock()

	tmpl, err := embeddedFiles.ReadFile("templates/admin.html")
	if err != nil {
		http.Error(w, "template error", 500)
		return
	}

	// Build user rows HTML
	var rows strings.Builder
	for _, u := range userList {
		rows.WriteString(fmt.Sprintf(
			`<tr><td>%s</td><td><button class="btn-delete" data-email="%s">Delete</button></td></tr>`,
			u.Email, u.Email,
		))
	}
	body := strings.ReplaceAll(string(tmpl), "{{.UserRows}}", rows.String())
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(body))
}

func handleAdminAddUser(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")
	if email == "" || password == "" {
		http.Error(w, "email and password required", 400)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}

	usersMu.Lock()
	for _, u := range users {
		if strings.EqualFold(u.Email, email) {
			usersMu.Unlock()
			http.Error(w, "user already exists", 409)
			return
		}
	}
	users = append(users, User{Email: email, PasswordHash: string(hash)})
	if err := saveUsers(); err != nil {
		usersMu.Unlock()
		http.Error(w, "failed to save users", 500)
		return
	}
	usersMu.Unlock()

	http.Redirect(w, r, "/admin", http.StatusFound)
}

func handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	email := r.PathValue("email")
	if email == "" {
		http.Error(w, "email required", 400)
		return
	}

	usersMu.Lock()
	newUsers := users[:0]
	found := false
	for _, u := range users {
		if strings.EqualFold(u.Email, email) {
			found = true
			continue
		}
		newUsers = append(newUsers, u)
	}
	if !found {
		usersMu.Unlock()
		http.Error(w, "user not found", 404)
		return
	}
	users = newUsers
	if err := saveUsers(); err != nil {
		usersMu.Unlock()
		http.Error(w, "failed to save users", 500)
		return
	}
	usersMu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

// ── WebSocket handlers ────────────────────────────────────────────────────────

func handleSenderWS(w http.ResponseWriter, r *http.Request) {
	s := getSession(r)
	if s == nil {
		http.Error(w, "unauthorized", 401)
		return
	}

	token := r.PathValue("token")
	room := getRoom(token)
	if room == nil || room.OwnerEmail != s.Email {
		http.Error(w, "room not found", 404)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("sender upgrade error: %v", err)
		return
	}

	room.SenderMu.Lock()
	room.SenderConn = conn
	room.SenderMu.Unlock()

	defer func() {
		conn.Close()
		room.SenderMu.Lock()
		room.SenderConn = nil
		room.SenderMu.Unlock()

		activeTransfers.Add(-1)
		room.Status = StatusComplete

		// Notify receiver
		room.ReceiverMu.Lock()
		if room.ReceiverConn != nil {
			room.ReceiverConn.WriteJSON(map[string]string{"type": "sender_disconnected"})
		}
		room.ReceiverMu.Unlock()
		deleteRoom(token)
	}()

	for {
		msgType, reader, err := conn.NextReader()
		if err != nil {
			break
		}

		if msgType == websocket.BinaryMessage {
			room.ReceiverMu.Lock()
			rc := room.ReceiverConn
			room.ReceiverMu.Unlock()

			if rc != nil {
				room.ReceiverMu.Lock()
				writer, err := rc.NextWriter(websocket.BinaryMessage)
				if err != nil {
					room.ReceiverMu.Unlock()
					break
				}
				io.Copy(writer, reader)
				writer.Close()
				room.ReceiverMu.Unlock()
			} else {
				// Drain the reader
				io.Copy(io.Discard, reader)
			}
		} else {
			// JSON control message
			data, err := io.ReadAll(reader)
			if err != nil {
				break
			}

			var msg map[string]any
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			msgTypeStr, _ := msg["type"].(string)
			switch msgTypeStr {
			case "ready":
				room.Status = StatusActive
				activeTransfers.Add(1)
				// Forward to receiver
				room.ReceiverMu.Lock()
				if room.ReceiverConn != nil {
					room.ReceiverConn.WriteJSON(msg)
				}
				room.ReceiverMu.Unlock()
			case "metadata":
				// Forward file metadata to receiver
				room.ReceiverMu.Lock()
				if room.ReceiverConn != nil {
					room.ReceiverConn.WriteJSON(msg)
				}
				room.ReceiverMu.Unlock()
			case "complete":
				room.ReceiverMu.Lock()
				if room.ReceiverConn != nil {
					room.ReceiverConn.WriteJSON(msg)
				}
				room.ReceiverMu.Unlock()
			}
		}
	}
}

func handleReceiverWS(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	room := getRoom(token)
	if room == nil {
		http.Error(w, "room not found", 404)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("receiver upgrade error: %v", err)
		return
	}

	room.ReceiverMu.Lock()
	room.ReceiverConn = conn
	room.ReceiverMu.Unlock()

	// Notify sender that receiver connected
	room.SenderMu.Lock()
	if room.SenderConn != nil {
		room.SenderConn.WriteJSON(map[string]string{"type": "receiver_connected"})
	}
	room.SenderMu.Unlock()

	defer func() {
		conn.Close()
		room.ReceiverMu.Lock()
		room.ReceiverConn = nil
		room.ReceiverMu.Unlock()

		// Notify sender
		room.SenderMu.Lock()
		if room.SenderConn != nil {
			room.SenderConn.WriteJSON(map[string]string{"type": "receiver_disconnected"})
		}
		room.SenderMu.Unlock()
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		// Forward control messages to sender
		room.SenderMu.Lock()
		if room.SenderConn != nil {
			room.SenderConn.WriteJSON(msg)
		}
		room.SenderMu.Unlock()
	}
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	// Load config from env
	port = os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	adminEmail = os.Getenv("ADMIN_EMAIL")
	if adminEmail == "" {
		adminEmail = "admin@localhost"
	}
	adminPassword = os.Getenv("ADMIN_PASSWORD")
	if adminPassword == "" {
		adminPassword = "changeme"
	}
	publicBaseURL = os.Getenv("PUBLIC_BASE_URL")
	dataDir = os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}
	if mc := os.Getenv("MAX_CONCURRENT"); mc != "" {
		fmt.Sscanf(mc, "%d", &maxConcurrent)
	}

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("cannot create data dir: %v", err)
	}

	loadUsers()
	go roomSweeper()

	mux := http.NewServeMux()

	// Static files
	mux.Handle("GET /static/", http.FileServer(http.FS(embeddedFiles)))

	// Public routes
	mux.HandleFunc("GET /{$}", handleRoot)
	mux.HandleFunc("GET /login", handleLoginGet)
	mux.HandleFunc("POST /login", handleLoginPost)
	mux.HandleFunc("GET /logout", handleLogout)
	mux.HandleFunc("GET /transfer/{token}", handleTransfer)

	// Authenticated routes
	mux.HandleFunc("GET /dashboard", requireAuth(handleDashboard))
	mux.HandleFunc("POST /dashboard/create-link", requireAuth(handleCreateLink))

	// Sender/receiver WebSocket
	mux.HandleFunc("GET /ws/dashboard/{token}", handleSenderWS)
	mux.HandleFunc("GET /ws/transfer/{token}", handleReceiverWS)

	// Admin routes
	mux.HandleFunc("GET /admin/login", handleAdminLoginGet)
	mux.HandleFunc("POST /admin/login", handleAdminLoginPost)
	mux.HandleFunc("GET /admin", requireAdmin(handleAdmin))
	mux.HandleFunc("POST /admin/users", requireAdmin(handleAdminAddUser))
	mux.HandleFunc("DELETE /admin/users/{email}", requireAdmin(handleAdminDeleteUser))

	log.Printf("chuck listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}
