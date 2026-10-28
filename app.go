package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	_ "github.com/lib/pq"
)

type Config struct {
	Port                string
	Env                 string
	SessionSecret       string
	AdminUsername       string
	AdminPassword       string
	DBHost              string
	DBPort              string
	DBUser              string
	DBPassword          string
	DBName              string
	DBSSLMode           string
	LLMProvider         string
	LLMBaseURL          string
	LLMAPIKey           string
	EmbeddingModel      string
	ChatModel           string
	EmbeddingDimensions int
	MaxUploadBytes      int64
}

type Document struct {
	ID             int
	Filename       string
	Category       string
	GroupNames     string
	MetadataDates  string
	MetadataPeople string
	FileSize       int64
	FileSizeText   string
	MimeType       string
	Status         string
	ChunkCount     int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Answer struct {
	Question  string           `json:"question"`
	Answer    string           `json:"answer"`
	Sources   []DocumentSource `json:"sources"`
	SessionID string           `json:"session_id,omitempty"`
}

type DocumentSource struct {
	Filename   string   `json:"filename"`
	Text       string   `json:"text"`
	Score      float64  `json:"score"`
	DocumentID int      `json:"document_id,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Dates      []string `json:"dates,omitempty"`
	People     []string `json:"people,omitempty"`
}

type server struct {
	db    *sql.DB
	cfg   Config
	tmpls *template.Template
	model ModelProvider
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("configuration loading failed: %v", err)
	}
	if err := validateConfig(cfg); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}
	log.Printf("Starting app on port %s", cfg.Port)

	db, err := openDB(cfg)
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	defer db.Close()

	if err := initDatabase(db); err != nil {
		log.Fatalf("database init failed: %v", err)
	}

	tmpls, err := template.ParseGlob("templates/*.html")
	if err != nil {
		log.Fatalf("template loading failed: %v", err)
	}
	model, err := newModelProvider(cfg)
	if err != nil {
		log.Fatalf("model provider configuration failed: %v", err)
	}
	s := &server{db: db, cfg: cfg, tmpls: tmpls, model: model}

	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.requireAuth(s.handleLogout))
	mux.HandleFunc("/documents", s.requireAuth(s.handleDocuments))
	mux.HandleFunc("/documents/new", s.requireAuth(s.handleUploadDocument))
	mux.HandleFunc("/documents/replace", s.requireAuth(s.handleReplaceDocument))
	mux.HandleFunc("/documents/delete", s.requireAuth(s.handleDeleteDocument))
	mux.HandleFunc("/categories", s.requireAuth(s.createCategory))
	mux.HandleFunc("/groups", s.requireAuth(s.createGroup))
	mux.HandleFunc("/groups/assign", s.requireAuth(s.assignGroup))
	mux.HandleFunc("/webpages", s.requireAuth(s.createWebPage))
	mux.HandleFunc("/ask", s.requireAuth(s.handleAsk))
	mux.HandleFunc("/api/ask", s.requireAuth(s.handleAskJSON))

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           securityHeaders(mux, cfg.Env == "production"),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      3 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stopped
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
		}
	}()

	log.Printf("Listening on %s", httpServer.Addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

func loadConfig() (Config, error) {
	port := getenv("APP_PORT", "8080")
	provider := strings.ToLower(getenv("LLM_PROVIDER", "openai"))
	baseURL := "https://api.openai.com/v1"
	embeddingModel := "text-embedding-3-small"
	chatModel := "gpt-4o-mini"
	embeddingDimensions := "1536"
	if provider == "ollama" {
		baseURL = "http://ollama:11434"
		embeddingModel = "nomic-embed-text"
		chatModel = "llama3.2"
		embeddingDimensions = "768"
	}
	cfg := Config{
		Port:           port,
		Env:            getenv("APP_ENV", "development"),
		SessionSecret:  getenv("SESSION_SECRET", "development-only-secret"),
		AdminUsername:  getenv("ADMIN_USERNAME", "admin"),
		AdminPassword:  getenv("ADMIN_PASSWORD", "change-me"),
		DBHost:         getenv("DB_HOST", "localhost"),
		DBPort:         getenv("DB_PORT", "5432"),
		DBUser:         getenv("DB_USER", "appuser"),
		DBPassword:     getenv("DB_PASSWORD", "appsecret"),
		DBName:         getenv("DB_NAME", "documentreview"),
		DBSSLMode:      getenv("DB_SSLMODE", "disable"),
		LLMProvider:    provider,
		LLMBaseURL:     getenv("LLM_BASE_URL", baseURL),
		LLMAPIKey:      getenv("LLM_API_KEY", ""),
		EmbeddingModel: getenv("EMBEDDING_MODEL", embeddingModel),
		ChatModel:      getenv("CHAT_MODEL", chatModel),
	}
	dimensions, err := strconv.Atoi(getenv("EMBEDDING_DIMENSIONS", embeddingDimensions))
	if err != nil || dimensions < 1 || dimensions > 16000 {
		return Config{}, errors.New("EMBEDDING_DIMENSIONS must be an integer from 1 to 16000")
	}
	cfg.EmbeddingDimensions = dimensions
	maxBytes, err := strconv.ParseInt(getenv("MAX_UPLOAD_BYTES", "5242880"), 10, 64)
	if err != nil || maxBytes <= 0 || maxBytes > 100*1024*1024 {
		return Config{}, errors.New("MAX_UPLOAD_BYTES must be an integer from 1 to 104857600")
	}
	cfg.MaxUploadBytes = maxBytes
	return cfg, nil
}

func validateConfig(cfg Config) error {
	if cfg.Port == "" || cfg.MaxUploadBytes < 1 || cfg.MaxUploadBytes > 100*1024*1024 ||
		cfg.EmbeddingDimensions < 1 || cfg.EmbeddingDimensions > 16000 ||
		cfg.DBHost == "" || cfg.DBPort == "" || cfg.DBUser == "" || cfg.DBName == "" {
		return errors.New("port, upload limit, and embedding dimensions must be valid")
	}
	port, err := strconv.Atoi(cfg.Port)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("APP_PORT must be a valid TCP port")
	}
	dbPort, err := strconv.Atoi(cfg.DBPort)
	if err != nil || dbPort < 1 || dbPort > 65535 {
		return errors.New("DB_PORT must be a valid TCP port")
	}
	if cfg.Env != "development" && cfg.Env != "production" {
		return errors.New("APP_ENV must be development or production")
	}
	if cfg.LLMProvider != "openai" && cfg.LLMProvider != "ollama" {
		return errors.New("LLM_PROVIDER must be openai or ollama")
	}
	if cfg.LLMBaseURL == "" || cfg.EmbeddingModel == "" || cfg.ChatModel == "" {
		return errors.New("model URL and model names are required")
	}
	parsedModelURL, err := url.Parse(cfg.LLMBaseURL)
	if err != nil || (parsedModelURL.Scheme != "http" && parsedModelURL.Scheme != "https") ||
		parsedModelURL.Host == "" || parsedModelURL.User != nil || parsedModelURL.RawQuery != "" || parsedModelURL.Fragment != "" {
		return errors.New("LLM_BASE_URL must be an absolute HTTP or HTTPS URL without embedded credentials")
	}
	if cfg.Env == "production" {
		if len(cfg.SessionSecret) < 32 || cfg.SessionSecret == "development-only-secret" || strings.HasPrefix(cfg.SessionSecret, "replace-with-") {
			return errors.New("SESSION_SECRET must be a unique random value of at least 32 characters in production")
		}
		if cfg.AdminUsername == "" || cfg.AdminUsername == "admin" ||
			cfg.AdminPassword == "" || cfg.AdminPassword == "change-me" || strings.HasPrefix(cfg.AdminPassword, "replace-with-") {
			return errors.New("set a non-default ADMIN_USERNAME and ADMIN_PASSWORD in production")
		}
		if cfg.DBPassword == "" || cfg.DBPassword == "appsecret" || strings.HasPrefix(cfg.DBPassword, "replace-with-") ||
			(cfg.DBSSLMode != "verify-ca" && cfg.DBSSLMode != "verify-full") {
			return errors.New("production requires a database password and DB_SSLMODE set to verify-ca or verify-full")
		}
		if cfg.LLMProvider == "openai" && cfg.LLMAPIKey == "" {
			return errors.New("LLM_API_KEY is required for the openai provider")
		}
		if cfg.LLMProvider == "openai" && parsedModelURL.Scheme != "https" {
			return errors.New("hosted model requests must use HTTPS in production")
		}
	}
	return nil
}

func getenv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}

func openDB(cfg Config) (*sql.DB, error) {
	dsnURL := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.DBUser, cfg.DBPassword),
		Host:   net.JoinHostPort(cfg.DBHost, cfg.DBPort),
		Path:   "/" + cfg.DBName,
	}
	query := url.Values{}
	query.Set("sslmode", cfg.DBSSLMode)
	dsnURL.RawQuery = query.Encode()
	dsn := dsnURL.String()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func initDatabase(db *sql.DB) error {
	_, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS vector;`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS documents (
			id SERIAL PRIMARY KEY,
			filename TEXT NOT NULL,
			content TEXT NOT NULL,
			file_size BIGINT NOT NULL,
			mime_type TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);`)
	if err != nil {
		return err
	}
	for _, statement := range []string{
		`ALTER TABLE documents ADD COLUMN IF NOT EXISTS category TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE documents ADD COLUMN IF NOT EXISTS metadata_dates TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE documents ADD COLUMN IF NOT EXISTS metadata_people TEXT NOT NULL DEFAULT '[]'`,
		`CREATE TABLE IF NOT EXISTS categories (name TEXT PRIMARY KEY, filename_keyword TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS document_groups (id SERIAL PRIMARY KEY, name TEXT NOT NULL UNIQUE)`,
		`CREATE TABLE IF NOT EXISTS document_group_members (document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE, group_id INTEGER NOT NULL REFERENCES document_groups(id) ON DELETE CASCADE, PRIMARY KEY(document_id,group_id))`,
		`CREATE TABLE IF NOT EXISTS web_pages (id SERIAL PRIMARY KEY, url TEXT NOT NULL UNIQUE, content TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE IF NOT EXISTS web_chunks (id SERIAL PRIMARY KEY, page_id INTEGER NOT NULL REFERENCES web_pages(id) ON DELETE CASCADE, chunk_index INTEGER NOT NULL, text TEXT NOT NULL, embedding VECTOR, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE INDEX IF NOT EXISTS web_chunks_page_idx ON web_chunks(page_id)`,
		`CREATE TABLE IF NOT EXISTS conversation_messages (id BIGSERIAL PRIMARY KEY, session_id TEXT NOT NULL, question TEXT NOT NULL, answer TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE INDEX IF NOT EXISTS conversation_messages_session_idx ON conversation_messages(session_id,id DESC)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chunks (
			id SERIAL PRIMARY KEY,
			document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
			chunk_index INTEGER NOT NULL,
			text TEXT NOT NULL,
			embedding VECTOR,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`DROP INDEX IF EXISTS chunks_embedding_idx;`)
	if err != nil {
		return err
	}
	var embeddingTypeModifier int
	if err := db.QueryRow(`
		SELECT atttypmod
		FROM pg_attribute
		WHERE attrelid = 'chunks'::regclass AND attname = 'embedding' AND NOT attisdropped
	`).Scan(&embeddingTypeModifier); err != nil {
		return err
	}
	if embeddingTypeModifier >= 0 {
		if _, err := db.Exec(`ALTER TABLE chunks ALTER COLUMN embedding TYPE VECTOR USING embedding::VECTOR;`); err != nil {
			return err
		}
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS chunks_document_idx ON chunks(document_id);`)
	if err != nil {
		return err
	}
	return nil
}

func (s *server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAuthenticated(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSONError(w, http.StatusUnauthorized, "Authentication is required.")
				return
			}
			redirectURL := "/login?next=" + url.QueryEscape(r.URL.RequestURI())
			http.Redirect(w, r, redirectURL, http.StatusSeeOther)
			return
		}
		if isMutation(r.Method) && !sameOriginRequest(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSONError(w, http.StatusForbidden, "Request origin could not be verified.")
				return
			}
			http.Error(w, "Request origin could not be verified.", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func isMutation(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

func sameOriginRequest(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		referer := r.Header.Get("Referer")
		if referer == "" {
			return false
		}
		parsed, err := url.Parse(referer)
		if err != nil {
			return false
		}
		origin = parsed.Scheme + "://" + parsed.Host
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host) &&
		(parsed.Scheme == "http" || parsed.Scheme == "https") &&
		parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func securityHeaders(next http.Handler, production bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'self'")
		if production {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if s.isAuthenticated(r) {
		http.Redirect(w, r, "/documents", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, "ok")
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.renderTemplate(w, "login.html", map[string]any{"Title": "Login", "Error": "", "Next": safeNext(r.URL.Query().Get("next"))})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sameOriginRequest(r) {
		http.Error(w, "Request origin could not be verified.", http.StatusForbidden)
		return
	}
	if err := parseFormBody(w, r, 64<<10); err != nil {
		http.Error(w, "The sign-in request could not be read.", http.StatusBadRequest)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	if username == "" || password == "" {
		s.renderTemplate(w, "login.html", map[string]any{"Title": "Login", "Error": "Username and password are required.", "Next": safeNext(r.FormValue("next"))})
		return
	}
	if subtle.ConstantTimeCompare([]byte(username), []byte(s.cfg.AdminUsername)) != 1 ||
		subtle.ConstantTimeCompare([]byte(password), []byte(s.cfg.AdminPassword)) != 1 {
		s.renderTemplate(w, "login.html", map[string]any{"Title": "Login", "Error": "Invalid username or password.", "Next": safeNext(r.FormValue("next"))})
		return
	}

	expires := time.Now().Add(8 * time.Hour)
	token := buildSessionToken(username, s.cfg.SessionSecret, expires)
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int((8 * time.Hour).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.Env == "production",
	})

	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cfg.Env == "production", SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *server) handleDocuments(w http.ResponseWriter, r *http.Request) {
	docs, err := s.listDocuments()
	if err != nil {
		log.Printf("document listing failed: %v", err)
		http.Error(w, "Unable to load documents. Please try again.", http.StatusInternalServerError)
		return
	}
	categories, err := s.categories()
	if err != nil {
		log.Printf("category listing failed: %v", err)
		http.Error(w, "Unable to load categories. Please try again.", http.StatusInternalServerError)
		return
	}
	groups, err := s.groups()
	if err != nil {
		log.Printf("group listing failed: %v", err)
		http.Error(w, "Unable to load groups. Please try again.", http.StatusInternalServerError)
		return
	}
	webPages, err := s.webPages()
	if err != nil {
		log.Printf("webpage listing failed: %v", err)
		http.Error(w, "Unable to load webpages. Please try again.", http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"Title": "Documents", "Documents": docs, "MaxUploadBytes": formatBytes(s.cfg.MaxUploadBytes),
		"MaxUploadBytesInt": s.cfg.MaxUploadBytes,
		"Provider":          s.cfg.LLMProvider,
		"Categories":        categories, "Groups": groups, "WebPages": webPages,
		"Notice": flashMessage(r.URL.Query().Get("status")), "Error": "",
	}
	s.renderTemplate(w, "documents.html", data)
}

func (s *server) handleUploadDocument(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	filename, text, size, mimeType, status, err := s.readUploadedText(w, r)
	if err != nil {
		s.renderDocumentError(w, err.Error(), status)
		return
	}
	if err := s.createCategorizedDocument(r.Context(), filename, text, mimeType, size); err != nil {
		log.Printf("document upload failed: %v", err)
		s.renderDocumentError(w, "The file could not be indexed and saved. Check the model or database settings and try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/documents?status=success", http.StatusSeeOther)
}

func (s *server) handleReplaceDocument(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if status, err := s.prepareMultipart(w, r); err != nil {
		s.renderDocumentError(w, err.Error(), status)
		return
	}
	id, err := strconv.Atoi(r.FormValue("id"))
	if err != nil {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
		s.renderDocumentError(w, "Select a valid document to replace.", http.StatusBadRequest)
		return
	}
	filename, text, size, mimeType, status, err := s.readUploadedText(w, r)
	if err != nil {
		s.renderDocumentError(w, err.Error(), status)
		return
	}
	if err := s.replaceCategorizedDocument(r.Context(), id, filename, text, mimeType, size); err != nil {
		log.Printf("document replacement failed for id %d: %v", id, err)
		if errors.Is(err, sql.ErrNoRows) {
			s.renderDocumentError(w, "That document no longer exists.", http.StatusNotFound)
			return
		}
		s.renderDocumentError(w, "The document could not be replaced. The previous version is unchanged; check the model or database settings and try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/documents?status=replaced", http.StatusSeeOther)
}

func (s *server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := parseFormBody(w, r, 64<<10); err != nil {
		s.renderDocumentError(w, "The request could not be read. Please try again.", http.StatusBadRequest)
		return
	}
	id, err := strconv.Atoi(r.FormValue("id"))
	if err != nil {
		s.renderDocumentError(w, "Select a valid document to remove.", http.StatusBadRequest)
		return
	}
	if err := s.deleteDocument(id); err != nil {
		log.Printf("document deletion failed for id %d: %v", id, err)
		if errors.Is(err, sql.ErrNoRows) {
			s.renderDocumentError(w, "That document no longer exists.", http.StatusNotFound)
			return
		}
		s.renderDocumentError(w, "Unable to remove the document. Please try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/documents?status=deleted", http.StatusSeeOther)
}

func (s *server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		session := ""
		if r.URL.Query().Get("new") != "1" {
			if cookie, err := r.Cookie("conversation"); err == nil && len(cookie.Value) == 48 && strings.Trim(cookie.Value, "0123456789abcdef") == "" {
				session = cookie.Value
			}
		}
		if session == "" {
			var err error
			session, err = newSessionID()
			if err != nil {
				http.Error(w, "Unable to start a local conversation.", http.StatusInternalServerError)
				return
			}
		}
		http.SetCookie(w, &http.Cookie{Name: "conversation", Value: session, Path: "/", MaxAge: 365 * 24 * 60 * 60, HttpOnly: true, Secure: s.cfg.Env == "production", SameSite: http.SameSiteLaxMode})
		s.renderAskPage(w, r, "", nil, "", session)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := parseFormBody(w, r, 64<<10); err != nil {
		s.renderAskPage(w, r, "", nil, "The question could not be read. Please try again.", "")
		return
	}
	question := strings.TrimSpace(r.FormValue("question"))
	if question == "" {
		s.renderAskPage(w, r, "", nil, "Ask a question to search the selected sources.", r.FormValue("session_id"))
		return
	}
	if utf8.RuneCountInString(question) > 4000 {
		s.renderAskPage(w, r, question, nil, "Keep your question to 4,000 characters or fewer.", r.FormValue("session_id"))
		return
	}
	options, err := askOptionsFromForm(r)
	if err != nil {
		s.renderAskPage(w, r, question, nil, "The source selection is invalid. Please try again.", "")
		return
	}
	if options.Session == "" {
		options.Session, err = newSessionID()
		if err != nil {
			http.Error(w, "Unable to start a local conversation.", http.StatusInternalServerError)
			return
		}
	}
	answer, err := s.answerQuestionWithOptions(r.Context(), question, options)
	if err != nil {
		log.Printf("question answering failed: %v", err)
		s.renderAskPage(w, r, question, nil, "The question could not be answered right now. Check the model settings and try again.", options.Session)
		return
	}
	s.renderAskPage(w, r, answer.Question, answer, "", options.Session)
}

func askOptionsFromForm(r *http.Request) (AskOptions, error) {
	options := AskOptions{
		Category:  strings.TrimSpace(r.FormValue("category")),
		Session:   r.FormValue("session_id"),
		SearchAll: r.FormValue("search_all") == "on",
	}
	if _, hasSearchAll := r.Form["search_all"]; !hasSearchAll && r.FormValue("selection_mode") != "interactive" && len(r.Form["file_ids"]) == 0 {
		options.SearchAll = true
	}
	if raw := r.FormValue("group_id"); raw != "" {
		groupID, err := strconv.Atoi(raw)
		if err != nil {
			return AskOptions{}, errors.New("invalid group")
		}
		options.GroupID = groupID
	}
	for _, raw := range r.Form["file_ids"] {
		id, err := strconv.Atoi(raw)
		if err != nil {
			return AskOptions{}, errors.New("invalid document")
		}
		options.Files = append(options.Files, id)
	}
	for _, raw := range r.Form["page_ids"] {
		id, err := strconv.Atoi(raw)
		if err != nil {
			return AskOptions{}, errors.New("invalid webpage")
		}
		options.Pages = append(options.Pages, id)
	}
	return options, validateAskOptions(options)
}

func validateAskOptions(options AskOptions) error {
	if options.Session != "" && (len(options.Session) != 48 || strings.Trim(options.Session, "0123456789abcdef") != "") {
		return errors.New("invalid conversation identifier")
	}
	if options.GroupID < 0 || len(options.Category) > 80 || len(options.Files) > 100 || len(options.Pages) > 50 {
		return errors.New("invalid source selection")
	}
	for _, id := range options.Files {
		if id < 1 {
			return errors.New("invalid document")
		}
	}
	for _, id := range options.Pages {
		if id < 1 {
			return errors.New("invalid webpage")
		}
	}
	return nil
}

func (s *server) renderAskPage(w http.ResponseWriter, r *http.Request, question string, answer *Answer, message, session string) {
	docs, err := s.listDocuments()
	if err != nil {
		log.Printf("ask page document listing failed: %v", err)
		http.Error(w, "Unable to load documents.", http.StatusInternalServerError)
		return
	}
	categories, err := s.categories()
	if err != nil {
		log.Printf("ask page category listing failed: %v", err)
		http.Error(w, "Unable to load categories.", http.StatusInternalServerError)
		return
	}
	groups, err := s.groups()
	if err != nil {
		log.Printf("ask page group listing failed: %v", err)
		http.Error(w, "Unable to load groups.", http.StatusInternalServerError)
		return
	}
	pages, err := s.webPages()
	if err != nil {
		log.Printf("ask page webpage listing failed: %v", err)
		http.Error(w, "Unable to load webpages.", http.StatusInternalServerError)
		return
	}
	if session == "" {
		session, err = newSessionID()
		if err != nil {
			http.Error(w, "Unable to start a local conversation.", http.StatusInternalServerError)
			return
		}
	}
	s.renderTemplate(w, "ask.html", map[string]any{
		"Title": "Ask a question", "Question": question, "Answer": answer, "Error": message,
		"Notice": "", "Documents": docs, "Categories": categories, "Groups": groups,
		"WebPages": pages, "SessionID": session,
	})
}

func (s *server) handleAskJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	question, options, err := readAskRequest(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "The question could not be read.")
		return
	}
	if question == "" {
		writeJSONError(w, http.StatusBadRequest, "Question is required.")
		return
	}
	if utf8.RuneCountInString(question) > 4000 {
		writeJSONError(w, http.StatusBadRequest, "Question must be 4,000 characters or fewer.")
		return
	}
	if options.Session == "" {
		options.Session, err = newSessionID()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "Unable to start a local conversation.")
			return
		}
	}
	answer, err := s.answerQuestionWithOptions(r.Context(), question, options)
	if err != nil {
		log.Printf("API question answering failed: %v", err)
		writeJSONError(w, http.StatusBadGateway, "Question could not be answered. Please try again.")
		return
	}
	answer.SessionID = options.Session
	http.SetCookie(w, &http.Cookie{Name: "conversation", Value: options.Session, Path: "/", MaxAge: 365 * 24 * 60 * 60, HttpOnly: true, Secure: s.cfg.Env == "production", SameSite: http.SameSiteLaxMode})
	writeJSON(w, answer)
}

func parseFormBody(w http.ResponseWriter, r *http.Request, maxBytes int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	return r.ParseForm()
}

func readAskRequest(w http.ResponseWriter, r *http.Request) (string, AskOptions, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var payload struct {
			Question  string `json:"question"`
			Category  string `json:"category"`
			GroupID   int    `json:"group_id"`
			FileIDs   []int  `json:"file_ids"`
			PageIDs   []int  `json:"page_ids"`
			SessionID string `json:"session_id"`
			SearchAll *bool  `json:"search_all"`
		}
		if err := decoder.Decode(&payload); err != nil {
			return "", AskOptions{}, err
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				return "", AskOptions{}, errors.New("request must contain one JSON value")
			}
			return "", AskOptions{}, err
		}
		options := AskOptions{
			Category: strings.TrimSpace(payload.Category), GroupID: payload.GroupID,
			Files: payload.FileIDs, Pages: payload.PageIDs, Session: payload.SessionID,
			SearchAll: payload.SearchAll == nil && len(payload.FileIDs) == 0,
		}
		if payload.SearchAll != nil {
			options.SearchAll = *payload.SearchAll
		}
		if err := validateAskOptions(options); err != nil {
			return "", AskOptions{}, err
		}
		return strings.TrimSpace(payload.Question), options, nil
	}
	if err := r.ParseForm(); err != nil {
		return "", AskOptions{}, err
	}
	options, err := askOptionsFromForm(r)
	return strings.TrimSpace(r.FormValue("question")), options, err
}

func (s *server) listDocuments() ([]Document, error) {
	rows, err := s.db.Query(`
		SELECT d.id, d.filename, d.file_size, d.mime_type, d.status, d.created_at, d.updated_at,
		       d.category, d.metadata_dates, d.metadata_people,
		       COALESCE((SELECT string_agg(g.name, ', ' ORDER BY g.name) FROM document_group_members gm JOIN document_groups g ON g.id=gm.group_id WHERE gm.document_id=d.id), ''),
		       COUNT(c.id) AS chunk_count
		FROM documents d
		LEFT JOIN chunks c ON c.document_id = d.id
		GROUP BY d.id
		ORDER BY d.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []Document
	for rows.Next() {
		d := Document{}
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&d.ID, &d.Filename, &d.FileSize, &d.MimeType, &d.Status, &createdAt, &updatedAt, &d.Category, &d.MetadataDates, &d.MetadataPeople, &d.GroupNames, &d.ChunkCount); err != nil {
			return nil, err
		}
		d.CreatedAt = createdAt
		d.UpdatedAt = updatedAt
		d.FileSizeText = formatBytes(d.FileSize)
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

func (s *server) readUploadedText(w http.ResponseWriter, r *http.Request) (string, string, int64, string, int, error) {
	if status, err := s.prepareMultipart(w, r); err != nil {
		return "", "", 0, "", status, err
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		return "", "", 0, "", http.StatusBadRequest, errors.New("Choose a supported file before submitting.")
	}
	defer file.Close()

	filename := strings.TrimSpace(strings.ReplaceAll(header.Filename, "\\", "/"))
	filename = filepath.Base(filename)
	if filename == "" || filename == "." || strings.ContainsRune(filename, '\x00') {
		return "", "", 0, "", http.StatusBadRequest, errors.New("The file name is invalid.")
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != ".txt" && ext != ".docx" && ext != ".xlsx" && ext != ".html" && ext != ".htm" {
		return "", "", 0, "", http.StatusBadRequest, errors.New("Supported file types are .docx, .xlsx, .html, .htm, and .txt.")
	}
	if header.Size > s.cfg.MaxUploadBytes {
		return "", "", 0, "", http.StatusRequestEntityTooLarge, errors.New("The upload is too large. Maximum file size is " + formatBytes(s.cfg.MaxUploadBytes) + ".")
	}
	contentBytes, err := io.ReadAll(io.LimitReader(file, s.cfg.MaxUploadBytes+1))
	if err != nil {
		return "", "", 0, "", http.StatusBadRequest, errors.New("The file could not be read.")
	}
	if int64(len(contentBytes)) > s.cfg.MaxUploadBytes {
		return "", "", 0, "", http.StatusRequestEntityTooLarge, errors.New("The upload is too large. Maximum file size is " + formatBytes(s.cfg.MaxUploadBytes) + ".")
	}
	if len(contentBytes) == 0 {
		return "", "", 0, "", http.StatusBadRequest, errors.New("The selected file is empty.")
	}
	text, mimeType, err := extractDocumentText(filename, contentBytes)
	if err != nil {
		return "", "", 0, "", http.StatusBadRequest, err
	}
	if text == "" {
		return "", "", 0, "", http.StatusBadRequest, errors.New("The selected file contains no readable text.")
	}
	if !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') {
		return "", "", 0, "", http.StatusBadRequest, errors.New("The extracted content is not valid UTF-8 text.")
	}
	if len(text) > 20<<20 {
		return "", "", 0, "", http.StatusRequestEntityTooLarge, errors.New("The extracted text exceeds the 20 MiB indexing limit.")
	}
	return filename, text, int64(len(contentBytes)), mimeType, http.StatusOK, nil
}

func (s *server) prepareMultipart(w http.ResponseWriter, r *http.Request) (int, error) {
	if r.MultipartForm != nil {
		return http.StatusOK, nil
	}
	maxRequestBytes := s.cfg.MaxUploadBytes + (1 << 20)
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return http.StatusRequestEntityTooLarge, errors.New("The upload is too large. Maximum file size is " + formatBytes(s.cfg.MaxUploadBytes) + ".")
		}
		return http.StatusBadRequest, errors.New("The upload could not be read. Choose a supported document and try again.")
	}
	return http.StatusOK, nil
}

func (s *server) createDocument(ctx context.Context, filename, content, mimeType string, size int64) error {
	processingCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	chunks, err := s.prepareChunks(processingCtx, content)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(processingCtx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var docID int
	err = tx.QueryRowContext(processingCtx, `INSERT INTO documents(filename, content, file_size, mime_type, status) VALUES($1, $2, $3, $4, 'ready') RETURNING id`, filename, content, size, mimeType).Scan(&docID)
	if err != nil {
		return err
	}
	if err := insertChunks(processingCtx, tx, docID, chunks); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *server) replaceDocument(ctx context.Context, id int, filename, content, mimeType string, size int64) error {
	processingCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	chunks, err := s.prepareChunks(processingCtx, content)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(processingCtx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(processingCtx, `UPDATE documents SET filename=$1, content=$2, file_size=$3, mime_type=$4, status='ready', updated_at=NOW() WHERE id=$5`, filename, content, size, mimeType, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(processingCtx, `DELETE FROM chunks WHERE document_id=$1`, id); err != nil {
		return err
	}
	if err := insertChunks(processingCtx, tx, id, chunks); err != nil {
		return err
	}
	return tx.Commit()
}

func insertChunks(ctx context.Context, tx *sql.Tx, documentID int, chunks []preparedChunk) error {
	for i, chunk := range chunks {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chunks(document_id, chunk_index, text, embedding) VALUES($1,$2,$3,$4::vector)`,
			documentID, i, chunk.text, vectorString(chunk.embedding)); err != nil {
			return fmt.Errorf("save chunk %d: %w", i, err)
		}
	}
	return nil
}

func (s *server) deleteDocument(id int) error {
	result, err := s.db.Exec(`DELETE FROM documents WHERE id = $1`, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *server) answerQuestion(parent context.Context, question string) (*Answer, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, errors.New("question is required")
	}
	if utf8.RuneCountInString(question) > 4000 {
		return nil, errors.New("question must be 4,000 characters or fewer")
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	embedding, err := s.model.Embed(ctx, question)
	if err != nil {
		return nil, fmt.Errorf("failed to generate embedding for your question: %w", err)
	}
	if err := s.validateEmbedding(embedding); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT c.text, d.filename, 1 - (c.embedding <=> $1::vector) AS score
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE c.embedding IS NOT NULL AND d.status = 'ready'
		ORDER BY c.embedding <=> $1::vector
		LIMIT 5`, vectorString(embedding))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sources []DocumentSource
	var context []string
	for rows.Next() {
		var text, filename string
		var score float64
		if err := rows.Scan(&text, &filename, &score); err != nil {
			return nil, err
		}
		sources = append(sources, DocumentSource{Filename: filename, Text: text, Score: score})
		context = append(context, text)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return &Answer{Question: question, Answer: "I couldn’t find enough relevant information in the uploaded files to answer that confidently.", Sources: nil}, nil
	}
	if sources[0].Score < 0.12 {
		return &Answer{Question: question, Answer: "I couldn’t find enough relevant information in the uploaded files to answer that confidently.", Sources: sources}, nil
	}
	answerText, err := s.model.Answer(ctx, question, context)
	if err != nil {
		return nil, err
	}
	return &Answer{Question: question, Answer: answerText, Sources: sources}, nil
}

func (s *server) prepareChunks(ctx context.Context, content string) ([]preparedChunk, error) {
	textChunks := splitTextIntoChunks(content, 500)
	if len(textChunks) == 0 {
		return nil, errors.New("document does not contain readable text")
	}
	prepared := make([]preparedChunk, 0, len(textChunks))
	for i, text := range textChunks {
		embedding, err := s.model.Embed(ctx, text)
		if err != nil {
			return nil, fmt.Errorf("embedding chunk %d: %w", i, err)
		}
		if err := s.validateEmbedding(embedding); err != nil {
			return nil, fmt.Errorf("embedding chunk %d: %w", i, err)
		}
		prepared = append(prepared, preparedChunk{text: text, embedding: embedding})
	}
	return prepared, nil
}

func (s *server) validateEmbedding(embedding []float32) error {
	if len(embedding) != s.cfg.EmbeddingDimensions {
		return fmt.Errorf("model returned %d embedding dimensions; configured for %d; reindex documents after changing embedding models", len(embedding), s.cfg.EmbeddingDimensions)
	}
	norm := float64(0)
	for _, value := range embedding {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return errors.New("model returned a non-finite embedding value")
		}
		norm += float64(value) * float64(value)
	}
	if norm == 0 {
		return errors.New("model returned an empty embedding vector")
	}
	return nil
}

func (s *server) isAuthenticated(r *http.Request) bool {
	cookie, err := r.Cookie("session")
	if err != nil {
		return false
	}
	return validateSessionToken(cookie.Value, s.cfg.SessionSecret, s.cfg.AdminUsername, time.Now())
}

func buildSessionToken(username, secret string, expires time.Time) string {
	payload := username + ":" + strconv.FormatInt(expires.Unix(), 10)
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write([]byte(payload))
	sig := hex.EncodeToString(h.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig
}

func validateSessionToken(token, secret, expectedUsername string, now time.Time) bool {
	if token == "" {
		return false
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	payload := string(decoded)
	usernameAndExpiry := strings.LastIndex(payload, ":")
	if usernameAndExpiry < 1 {
		return false
	}
	username := payload[:usernameAndExpiry]
	if username != expectedUsername {
		return false
	}
	expiry, err := strconv.ParseInt(payload[usernameAndExpiry+1:], 10, 64)
	if err != nil || expiry <= now.Unix() {
		return false
	}
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write([]byte(payload))
	expected := hex.EncodeToString(h.Sum(nil))
	return hmac.Equal([]byte(parts[1]), []byte(expected))
}

func safeNext(candidate string) string {
	if candidate == "" || !strings.HasPrefix(candidate, "/") || strings.HasPrefix(candidate, "//") || strings.Contains(candidate, "\\") {
		return "/documents"
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") {
		return "/documents"
	}
	return parsed.RequestURI()
}

func flashMessage(status string) string {
	switch status {
	case "success":
		return "Document uploaded, categorized, and indexed successfully."
	case "replaced":
		return "Document replaced successfully. Its new version is ready to search."
	case "deleted":
		return "Document and its searchable content were removed."
	case "category":
		return "Category rule saved and matching files updated."
	case "group":
		return "Document group created."
	case "assigned":
		return "Document added to the group."
	case "unassigned":
		return "Document removed from the group."
	case "webpage":
		return "Webpage fetched and indexed."
	default:
		return ""
	}
}

func (s *server) renderDocumentError(w http.ResponseWriter, message string, status int) {
	docs, err := s.listDocuments()
	if err != nil {
		log.Printf("document listing after failed operation failed: %v", err)
		http.Error(w, "The operation failed and documents could not be loaded.", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	s.renderTemplate(w, "documents.html", map[string]any{
		"Title": "Documents", "Documents": docs, "MaxUploadBytes": formatBytes(s.cfg.MaxUploadBytes),
		"MaxUploadBytesInt": s.cfg.MaxUploadBytes,
		"Provider":          s.cfg.LLMProvider,
		"Error":             message, "Notice": "",
	})
}

func splitTextIntoChunks(content string, maxLen int) []string {
	if maxLen < 1 {
		return nil
	}
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil
	}
	paragraphs := strings.FieldsFunc(trimmed, func(r rune) bool { return r == '\n' || r == '\r' })
	chunks := make([]string, 0, len(paragraphs))
	var current strings.Builder
	for _, paragraph := range paragraphs {
		cleaned := strings.TrimSpace(paragraph)
		if cleaned == "" {
			continue
		}
		if current.Len() > 0 && current.Len()+len(cleaned)+1 > maxLen {
			chunks = append(chunks, strings.TrimSpace(current.String()))
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteString("\n")
		}
		current.WriteString(cleaned)
	}
	if current.Len() > 0 {
		chunks = append(chunks, strings.TrimSpace(current.String()))
	}
	if len(chunks) == 0 {
		chunks = []string{trimmed}
	}
	result := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		if len(chunk) > maxLen {
			result = append(result, splitLongChunk(chunk, maxLen)...)
		} else {
			result = append(result, chunk)
		}
	}
	return result
}

func splitLongChunk(chunk string, maxLen int) []string {
	parts := make([]string, 0, (len(chunk)+maxLen-1)/maxLen)
	for start := 0; start < len(chunk); {
		end := start
		for end < len(chunk) {
			_, size := utf8.DecodeRuneInString(chunk[end:])
			if end > start && end-start+size > maxLen {
				break
			}
			end += size
			if end-start >= maxLen {
				break
			}
		}
		parts = append(parts, chunk[start:end])
		start = end
	}
	return parts
}

func vectorString(values []float32) string {
	if len(values) == 0 {
		return "[]"
	}
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.FormatFloat(float64(value), 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func formatBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	sizeFloat := float64(size)
	for _, suffix := range []string{"KB", "MB", "GB"} {
		sizeFloat /= unit
		if sizeFloat < unit {
			return fmt.Sprintf("%.1f %s", sizeFloat, suffix)
		}
	}
	return fmt.Sprintf("%.1f TB", sizeFloat)
}

func (s *server) renderTemplate(w http.ResponseWriter, templateName string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.tmpls.ExecuteTemplate(w, templateName, data); err != nil {
		log.Printf("render %s failed: %v", templateName, err)
		http.Error(w, "Unable to render this page.", http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("JSON response encoding failed: %v", err)
		http.Error(w, "JSON encode error", http.StatusInternalServerError)
	}
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil {
		log.Printf("JSON error response encoding failed: %v", err)
	}
}
