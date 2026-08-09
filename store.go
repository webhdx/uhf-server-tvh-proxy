package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type recordingCreate struct {
	Name              string         `json:"name"`
	URL               string         `json:"url"`
	StartTime         time.Time      `json:"start_time"`
	DurationSeconds   int64          `json:"duration_seconds"`
	Description       *string        `json:"description,omitempty"`
	ParentID          *string        `json:"parent_id,omitempty"`
	Headers           *string        `json:"headers,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
	RecurrenceDays    []int          `json:"recurrence_days,omitempty"`
	RecurrenceEndDate *time.Time     `json:"recurrence_end_date,omitempty"`
}

type storedRecording struct {
	ID             string          `json:"id"`
	TVHUUID        string          `json:"tvh_uuid"`
	CreatedAt      time.Time       `json:"created_at"`
	Request        recordingCreate `json:"request"`
	StatusOverride string          `json:"status_override,omitempty"`
}

type storeFile struct {
	Recordings []storedRecording `json:"recordings"`
}

type recordingStore struct {
	mu         sync.RWMutex
	path       string
	recordings map[string]storedRecording
}

func openStore(path string) (*recordingStore, error) {
	s := &recordingStore{path: path, recordings: make(map[string]storedRecording)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var file storeFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	for _, recording := range file.Recordings {
		s.recordings[recording.ID] = recording
	}
	return s, nil
}

func (s *recordingStore) list() []storedRecording {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]storedRecording, 0, len(s.recordings))
	for _, recording := range s.recordings {
		result = append(result, recording)
	}
	return result
}

func (s *recordingStore) get(id string) (storedRecording, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	recording, ok := s.recordings[id]
	return recording, ok
}

func (s *recordingStore) put(recording storedRecording) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordings[recording.ID] = recording
	return s.saveLocked()
}

func (s *recordingStore) delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recordings, id)
	return s.saveLocked()
}

func (s *recordingStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	file := storeFile{Recordings: make([]storedRecording, 0, len(s.recordings))}
	for _, recording := range s.recordings {
		file.Recordings = append(file.Recordings, recording)
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := os.Rename(temporary, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}
