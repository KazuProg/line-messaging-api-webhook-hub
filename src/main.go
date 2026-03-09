package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	defaultPort        = "80"
	defaultClientsFile = "/data/clients.json"
	maxRequestBodySize = 1 << 20 // 1MB
	maxClientsBodySize = 64 << 10 // 64KB
)

type webhookPayload struct {
	Events []struct {
		WebhookEventID string `json:"webhookEventId"`
	} `json:"events"`
}

type appConfig struct {
	Port            string
	ClientsFilePath string
	ChannelSecret   string
	Logger          *slog.Logger
	Clients         *clientStore
	HTTPClient      *http.Client
	RequestTimeout  time.Duration
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

	cfg.HTTPClient = &http.Client{
		Timeout: cfg.RequestTimeout,
	}

	clients, err := newClientStore(cfg.ClientsFilePath, logger)
	if err != nil {
		logger.Error("failed to load clients store", "error", err.Error())
		os.Exit(1)
	}
	cfg.Clients = clients

	mux := http.NewServeMux()
	mux.Handle("/callback", callbackHandler(cfg))
	mux.Handle("/health", healthHandler())
	mux.Handle("/clients", clientsHandler(cfg))

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

	clientsFile := os.Getenv("CLIENTS_FILE")
	if clientsFile == "" {
		clientsFile = defaultClientsFile
	}

	secret := os.Getenv("LINE_CHANNEL_SECRET")
	if secret == "" {
		logger.Warn("LINE_CHANNEL_SECRET is not set; signature verification will always fail")
	}

	return &appConfig{
		Port:            port,
		ClientsFilePath: clientsFile,
		ChannelSecret:   secret,
		Logger:          logger,
		RequestTimeout:  5 * time.Second,
	}, nil
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

		if !verifySignature(cfg.ChannelSecret, sig, body, cfg.Logger) {
			cfg.Logger.Warn("signature verification failed")
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}

		clients := cfg.Clients.List()
		if len(clients) == 0 {
			cfg.Logger.Warn("no webhook clients registered; rejecting webhook")
			http.Error(w, "no webhook clients registered", http.StatusServiceUnavailable)
			return
		}

		eventID := extractEventID(body)
		var required, optional []Client
		for _, c := range clients {
			if c.Required {
				required = append(required, c)
			} else {
				optional = append(optional, c)
			}
		}

		if len(required) > 0 {
			if err := forwardToRequiredSync(cfg, eventID, body, required); err != nil {
				cfg.Logger.Error("required client delivery failed", "error", err.Error(), "event_id", eventID)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}

		if len(optional) > 0 {
			go forwardToClients(cfg, eventID, body, optional)
		}

		w.WriteHeader(http.StatusOK)
	})
}

func healthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}

type clientRequest struct {
	WebhookURL string `json:"webhook_url"`
	Required   bool   `json:"required"` // if true, any failure to this client causes LINE to receive 5xx (retry)
}

const clientsAllowMethods = "GET, POST, DELETE"

func clientsHandler(cfg *appConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")

		switch r.Method {
		case http.MethodGet:
			list := cfg.Clients.List()
			if list == nil {
				list = []Client{}
			}
			_ = json.NewEncoder(w).Encode(list)

		case http.MethodPost:
			r.Body = http.MaxBytesReader(w, r.Body, maxClientsBodySize)
			var req clientRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid JSON"}`))
				return
			}
			if req.WebhookURL == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"webhook_url is required"}`))
				return
			}
			_, err := cfg.Clients.Add(req.WebhookURL, req.Required)
			if err != nil {
				cfg.Logger.Error("failed to add client", "error", err.Error())
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal error"}`))
				return
			}
			cfg.Logger.Info("client registered", "webhook_url", req.WebhookURL, "required", req.Required)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"webhook_url": req.WebhookURL, "required": req.Required})

		case http.MethodDelete:
			r.Body = http.MaxBytesReader(w, r.Body, maxClientsBodySize)
			var req clientRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid JSON"}`))
				return
			}
			if req.WebhookURL == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"webhook_url is required"}`))
				return
			}
			removed, err := cfg.Clients.Remove(req.WebhookURL)
			if err != nil {
				cfg.Logger.Error("failed to remove client", "error", err.Error())
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal error"}`))
				return
			}
			if !removed {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"client not found"}`))
				return
			}
			cfg.Logger.Info("client removed", "webhook_url", req.WebhookURL)
			w.WriteHeader(http.StatusNoContent)

		default:
			w.Header().Set("Allow", clientsAllowMethods)
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		}
	})
}

func verifySignature(secret, signature string, body []byte, logger *slog.Logger) bool {
	if secret == "" {
		// Without secret, verification is meaningless; fail closed.
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write(body); err != nil {
		logger.Error("failed to compute hmac", "error", err)
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

func forwardToRequiredSync(cfg *appConfig, eventID string, body []byte, required []Client) error {
	var wg sync.WaitGroup
	var firstErrMu sync.Mutex
	var firstErr error
	for _, c := range required {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, io.NopCloser(bytes.NewReader(body)))
			if err != nil {
				cfg.Logger.Error("failed to create forwarding request", "error", err.Error(), "event_id", eventID, "url", url)
				firstErrMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				firstErrMu.Unlock()
				return
			}
			req.Header.Set("Content-Type", "application/json; charset=utf-8")

			resp, err := cfg.HTTPClient.Do(req)
			if err != nil {
				cfg.Logger.Error("forwarding failed", "error", err.Error(), "event_id", eventID, "url", url)
				firstErrMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				firstErrMu.Unlock()
				return
			}
			_ = resp.Body.Close()

			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				cfg.Logger.Info("forwarding success", "event_id", eventID, "url", url)
			} else {
				cfg.Logger.Error("forwarding failed", "event_id", eventID, "url", url, "status", resp.StatusCode)
				firstErrMu.Lock()
				if firstErr == nil {
					firstErr = errors.New("required client returned non-2xx: " + strconv.Itoa(resp.StatusCode))
				}
				firstErrMu.Unlock()
			}
		}(c.WebhookURL)
	}
	wg.Wait()
	return firstErr
}

func forwardToClients(cfg *appConfig, eventID string, body []byte, clients []Client) {
	for _, c := range clients {
		go func(url string) {
			ctx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, io.NopCloser(bytes.NewReader(body)))
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
		}(c.WebhookURL)
	}
}

