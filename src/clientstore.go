package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"sync"
)

type clientStore struct {
	mu       sync.RWMutex
	urls     []string
	filePath string
	logger   *slog.Logger
}

func newClientStore(filePath string, logger *slog.Logger) (*clientStore, error) {
	s := &clientStore{filePath: filePath, logger: logger, urls: []string{}}
	if err := s.load(); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		logger.Info("clients file not found; starting with empty list", "path", filePath)
	}
	return s, nil
}

func (s *clientStore) load() error {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		return err
	}
	var urls []string
	if err := json.Unmarshal(data, &urls); err != nil {
		return err
	}
	s.mu.Lock()
	s.urls = urls
	s.mu.Unlock()
	s.logger.Info("clients loaded", "path", s.filePath, "count", len(urls))
	return nil
}

func (s *clientStore) persist() error {
	s.mu.RLock()
	urls := make([]string, len(s.urls))
	copy(urls, s.urls)
	s.mu.RUnlock()

	data, err := json.MarshalIndent(urls, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.filePath, data, 0644)
}

func (s *clientStore) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.urls))
	copy(out, s.urls)
	return out
}

func (s *clientStore) Add(url string) (added bool, err error) {
	s.mu.Lock()
	for _, u := range s.urls {
		if u == url {
			s.mu.Unlock()
			return false, nil
		}
	}
	s.urls = append(s.urls, url)
	s.mu.Unlock()
	return true, s.persist()
}

func (s *clientStore) Remove(url string) (removed bool, err error) {
	s.mu.Lock()
	for i, u := range s.urls {
		if u == url {
			s.urls = append(s.urls[:i], s.urls[i+1:]...)
			s.mu.Unlock()
			return true, s.persist()
		}
	}
	s.mu.Unlock()
	return false, nil
}
