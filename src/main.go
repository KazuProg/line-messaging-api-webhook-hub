package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	defaultPort        = "80"
	defaultDBPath      = "/data/webhook.db"
	maxRequestBodySize = 1 << 20 // 1MB
)

type webhookPayload struct {
	Events []struct {
		WebhookEventID string `json:"webhookEventId"`
	} `json:"events"`
}

type appConfig struct {
	Port           string
	DBPath         string
	ChannelSecret  string
	Logger         *slog.Logger
	DB             *sql.DB
	HTTPClient     *http.Client
	RequestTimeout time.Duration
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := newConfig(logger)
	if err != nil {
		logger.Error("failed to load config", "error", err.Error())
		os.Exit(1)
	}

	db, err := openDB(cfg.DBPath, logger)
	if err != nil {
		logger.Error("failed to open database", "error", err.Error())
		os.Exit(1)
	}
	defer db.Close()

	cfg.DB = db
	cfg.HTTPClient = &http.Client{
		Timeout: cfg.RequestTimeout,
	}

	if err := initSchema(db, logger); err != nil {
		logger.Error("failed to initialize database schema", "error", err.Error())
		os.Exit(1)
	}

	if err := warnIfNoClients(db, logger); err != nil {
		logger.Error("failed to check clients table", "error", err.Error())
	}

	mux := http.NewServeMux()
	mux.Handle("/callback", callbackHandler(cfg))
	mux.Handle("/health", healthHandler())

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	logger.Info("starting server", "port", cfg.Port)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server error", "error", err.Error())
		os.Exit(1)
	}
}

func newConfig(logger *slog.Logger) (*appConfig, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = defaultDBPath
	}

	secret := os.Getenv("LINE_CHANNEL_SECRET")
	if secret == "" {
		logger.Warn("LINE_CHANNEL_SECRET is not set; signature verification will always fail")
	}

	return &appConfig{
		Port:           port,
		DBPath:         dbPath,
		ChannelSecret:  secret,
		Logger:         logger,
		RequestTimeout: 5 * time.Second,
	}, nil
}

func openDB(path string, logger *slog.Logger) (*sql.DB, error) {
	// Enable WAL mode via connection string pragma.
	dsn := path + "?_journal_mode=WAL&_foreign_keys=on"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}

	logger.Info("database opened", "path", path)
	return db, nil
}

func initSchema(db *sql.DB, logger *slog.Logger) error {
	schema := `
CREATE TABLE IF NOT EXISTS webhooks (
	id TEXT PRIMARY KEY,
	payload TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS clients (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	webhook_url TEXT NOT NULL,
	is_active INTEGER DEFAULT 1
);`

	if _, err := db.Exec(schema); err != nil {
		return err
	}
	logger.Info("database schema initialized")
	return nil
}

func warnIfNoClients(db *sql.DB, logger *slog.Logger) error {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM clients WHERE is_active = 1`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		logger.Warn("no active clients found; webhook events will be archived only")
	}
	return nil
}

func callbackHandler(cfg *appConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}

		sig := r.Header.Get("x-line-signature")
		if sig == "" {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		defer r.Body.Close()

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}

		if !verifySignature(cfg.ChannelSecret, sig, body) {
			cfg.Logger.Warn("signature verification failed")
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}

		eventID := extractEventID(body)
		if err := archiveWebhook(cfg.DB, eventID, string(body)); err != nil {
			cfg.Logger.Error("failed to archive webhook", "error", err.Error(), "event_id", eventID)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}

		go forwardToClients(cfg, eventID, body)

		w.WriteHeader(http.StatusOK)
	})
}

func healthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}

func verifySignature(secret, signature string, body []byte) bool {
	if secret == "" {
		// Without secret, verification is meaningless; fail closed.
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write(body); err != nil {
		log.Printf("failed to compute hmac: %v", err)
		return false
	}
	expected := mac.Sum(nil)
	expectedBase64 := base64.StdEncoding.EncodeToString(expected)

	// Compare in constant time.
	return hmac.Equal([]byte(expectedBase64), []byte(signature))
}

func extractEventID(body []byte) string {
	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return fallbackEventID()
	}
	if len(payload.Events) == 0 {
		return fallbackEventID()
	}
	if payload.Events[0].WebhookEventID == "" {
		return fallbackEventID()
	}
	return payload.Events[0].WebhookEventID
}

func fallbackEventID() string {
	ms := time.Now().UnixNano() / int64(time.Millisecond)
	return "unknown-" + strconv.FormatInt(ms, 10)
}

func archiveWebhook(db *sql.DB, id, payload string) error {
	_, err := db.Exec(`INSERT OR IGNORE INTO webhooks (id, payload) VALUES (?, ?)`, id, payload)
	return err
}

func forwardToClients(cfg *appConfig, eventID string, body []byte) {
	rows, err := cfg.DB.Query(`SELECT webhook_url FROM clients WHERE is_active = 1`)
	if err != nil {
		cfg.Logger.Error("failed to query clients", "error", err.Error(), "event_id", eventID)
		return
	}
	defer rows.Close()

	type client struct {
		URL string
	}
	var clients []client
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			cfg.Logger.Error("failed to scan client row", "error", err.Error(), "event_id", eventID)
			continue
		}
		clients = append(clients, client{URL: url})
	}

	for _, c := range clients {
		go func(url string) {
			ctx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, io.NopCloser(io.LimitReader(io.MultiReader(bytesReader(body)), int64(len(body)))))
			if err != nil {
				cfg.Logger.Error("failed to create forwarding request", "error", err.Error(), "event_id", eventID, "url", url)
				return
			}
			req.Header.Set("Content-Type", "application/json; charset=utf-8")

			resp, err := cfg.HTTPClient.Do(req)
			if err != nil {
				cfg.Logger.Error("forwarding failed", "error", err.Error(), "event_id", eventID, "url", url)
				return
			}
			_ = resp.Body.Close()

			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				cfg.Logger.Info("forwarding success", "event_id", eventID, "url", url)
			} else {
				cfg.Logger.Error("forwarding failed", "event_id", eventID, "url", url, "status", resp.StatusCode)
			}
		}(c.URL)
	}
}

// bytesReader returns an io.Reader over the given byte slice without extra allocation.
func bytesReader(b []byte) io.Reader {
	return &byteSliceReader{b: b}
}

type byteSliceReader struct {
	b []byte
	i int64
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.i >= int64(len(r.b)) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += int64(n)
	return n, nil
}
