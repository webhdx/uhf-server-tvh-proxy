package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const emulatedVersion = "1.6.0"

type config struct {
	ListenAddr     string
	TVHURL         string
	TVHUsername    string
	TVHPassword    string
	StatePath      string
	ServerPassword string
}

type server struct {
	cfg       config
	tvh       *tvhClient
	store     *recordingStore
	startedAt time.Time
	tokensMu  sync.Mutex
	tokens    map[string]time.Time
}

func newServer(cfg config, tvh *tvhClient, store *recordingStore) *server {
	return &server{cfg: cfg, tvh: tvh, store: store, startedAt: time.Now(), tokens: make(map[string]time.Time)}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/login", s.login)
	mux.HandleFunc("GET /server/stats", s.stats)
	mux.HandleFunc("GET /dvr/recordings", s.auth(s.listRecordings))
	mux.HandleFunc("POST /dvr/recordings", s.auth(s.createRecording))
	mux.HandleFunc("GET /dvr/recordings/{id}", s.auth(s.getRecording))
	mux.HandleFunc("DELETE /dvr/recordings/{id}", s.auth(s.deleteRecording))
	mux.HandleFunc("GET /dvr/recordings/{id}/metadata", s.auth(s.getMetadata))
	mux.HandleFunc("PATCH /dvr/recordings/{id}/metadata", s.auth(s.updateMetadata))
	mux.HandleFunc("GET /dvr/recordings/{id}/stream", s.auth(s.streamRecording))
	mux.HandleFunc("GET /dvr/recordings/{id}/thumbnail", s.thumbnail)
	mux.HandleFunc("GET /dvr/recordings/{id}/commercials", s.auth(s.commercials))
	mux.HandleFunc("PATCH /dvr/recordings/{id}/cancel", s.auth(s.cancelRecording))
	mux.HandleFunc("PATCH /dvr/recordings/{id}/cancel-recurrence", s.auth(s.cancelRecurrence))
	return s.logRequests(mux)
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var credentials struct {
		Email          string  `json:"email"`
		Password       string  `json:"password"`
		ServerPassword *string `json:"server_password"`
		DeviceID       string  `json:"device_id"`
	}
	if err := decodeJSON(r, &credentials); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if credentials.Email == "" || credentials.Password == "" || credentials.DeviceID == "" {
		writeError(w, http.StatusUnprocessableEntity, "email, password and device_id are required")
		return
	}
	if s.cfg.ServerPassword != "" && (credentials.ServerPassword == nil || *credentials.ServerPassword != s.cfg.ServerPassword) {
		writeError(w, http.StatusUnauthorized, "Invalid server password")
		return
	}
	token, err := randomToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not create token")
		return
	}
	expiresAt := time.Now().Add(24 * time.Hour)
	s.tokensMu.Lock()
	s.tokens[token] = expiresAt
	s.tokensMu.Unlock()
	refreshToken, _ := randomToken()
	writeJSON(w, http.StatusOK, map[string]any{
		"id_token": token, "refresh_token": refreshToken, "expires_in": 86400, "device_id": credentials.DeviceID,
	})
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		if !strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
			writeError(w, http.StatusForbidden, "Not authenticated")
			return
		}
		token := strings.TrimSpace(authorization[len("Bearer "):])
		if !s.validToken(token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "Invalid or expired token")
			return
		}
		next(w, r)
	}
}

func (s *server) validToken(token string) bool {
	s.tokensMu.Lock()
	defer s.tokensMu.Unlock()
	expiresAt, ok := s.tokens[token]
	if ok && time.Now().After(expiresAt) {
		delete(s.tokens, token)
		return false
	}
	return ok
}

func (s *server) createRecording(w http.ResponseWriter, r *http.Request) {
	var request recordingCreate
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if request.Name == "" || request.URL == "" || request.StartTime.IsZero() || request.DurationSeconds <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "name, url, start_time and a positive duration_seconds are required")
		return
	}
	if len(request.RecurrenceDays) != 0 {
		writeError(w, http.StatusNotImplemented, "Recurring recordings are not supported yet")
		return
	}

	now := time.Now().UTC()
	end := request.StartTime.Add(time.Duration(request.DurationSeconds) * time.Second)
	if request.StartTime.Before(now) {
		if !end.After(now) {
			writeError(w, http.StatusUnprocessableEntity, "Recording end time is in the past")
			return
		}
		request.StartTime = now
		request.DurationSeconds = int64(end.Sub(now).Seconds())
	}
	tvhUUID, err := s.tvh.createRecording(r.Context(), request)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	id, err := randomUUID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not create recording ID")
		return
	}
	stored := storedRecording{ID: id, TVHUUID: tvhUUID, CreatedAt: now, Request: request}
	if err := s.store.put(stored); err != nil {
		_ = s.tvh.cancel(r.Context(), tvhUUID)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, recordingResponse(stored, nil, now))
}

func (s *server) listRecordings(w http.ResponseWriter, r *http.Request) {
	entries, err := s.tvh.recordings(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	now := time.Now().UTC()
	stored := s.store.list()
	sort.Slice(stored, func(i, j int) bool { return stored[i].Request.StartTime.Before(stored[j].Request.StartTime) })
	result := make([]map[string]any, 0, len(stored))
	for _, recording := range stored {
		result = append(result, recordingResponse(recording, entries[recording.TVHUUID], now))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) getRecording(w http.ResponseWriter, r *http.Request) {
	recording, ok := s.store.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Recording not found")
		return
	}
	entries, err := s.tvh.recordings(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, recordingResponse(recording, entries[recording.TVHUUID], time.Now().UTC()))
}

func (s *server) cancelRecording(w http.ResponseWriter, r *http.Request) {
	recording, ok := s.store.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Recording not found")
		return
	}
	if recording.StatusOverride != "cancelled" {
		if err := s.tvh.cancel(r.Context(), recording.TVHUUID); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		recording.StatusOverride = "cancelled"
		if err := s.store.put(recording); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, recordingResponse(recording, nil, time.Now().UTC()))
}

func (s *server) deleteRecording(w http.ResponseWriter, r *http.Request) {
	recording, ok := s.store.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Recording not found")
		return
	}
	entries, err := s.tvh.recordings(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	response := recordingResponse(recording, entries[recording.TVHUUID], time.Now().UTC())
	status, _ := response["status"].(string)
	if status == "completed" || status == "failed" {
		err = s.tvh.remove(r.Context(), recording.TVHUUID)
	} else if status != "cancelled" {
		err = s.tvh.cancel(r.Context(), recording.TVHUUID)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := s.store.delete(recording.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) getMetadata(w http.ResponseWriter, r *http.Request) {
	recording, ok := s.store.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Recording not found")
		return
	}
	if recording.Request.Metadata == nil {
		recording.Request.Metadata = map[string]any{}
	}
	writeJSON(w, http.StatusOK, recording.Request.Metadata)
}

func (s *server) updateMetadata(w http.ResponseWriter, r *http.Request) {
	recording, ok := s.store.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Recording not found")
		return
	}
	var update struct {
		Metadata map[string]any `json:"metadata"`
	}
	if err := decodeJSON(r, &update); err != nil || update.Metadata == nil {
		writeError(w, http.StatusUnprocessableEntity, "metadata is required")
		return
	}
	if recording.Request.Metadata == nil {
		recording.Request.Metadata = make(map[string]any)
	}
	for key, value := range update.Metadata {
		if value == nil {
			delete(recording.Request.Metadata, key)
		} else {
			recording.Request.Metadata[key] = value
		}
	}
	if err := s.store.put(recording); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, recordingResponse(recording, nil, time.Now().UTC()))
}

func (s *server) streamRecording(w http.ResponseWriter, r *http.Request) {
	recording, ok := s.store.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Recording not found")
		return
	}
	response, err := s.tvh.stream(r.Context(), recording.TVHUUID, r)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer response.Body.Close()
	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func (s *server) thumbnail(w http.ResponseWriter, r *http.Request) {
	if token := r.URL.Query().Get("token"); token != "" {
		if !s.validToken(token) {
			writeError(w, http.StatusForbidden, "Invalid or expired token")
			return
		}
		writeError(w, http.StatusNotFound, "Thumbnail not available")
		return
	}
	if r.Header.Get("Authorization") != "" {
		s.auth(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, "Thumbnail not available")
		})(w, r)
		return
	}
	writeError(w, http.StatusUnauthorized, "Authentication required (pass token as query param or Authorization header)")
}

func (s *server) commercials(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"commercials": []any{}, "total_segments": 0})
}

func (s *server) cancelRecurrence(w http.ResponseWriter, r *http.Request) {
	recording, ok := s.store.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Recording not found")
		return
	}
	if len(recording.Request.RecurrenceDays) == 0 {
		writeError(w, http.StatusBadRequest, "Recording does not have a recurrence schedule")
		return
	}
}

func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	if err := s.tvh.ping(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	disk := diskStats("/")
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	recordingsDisk := disk
	recordingsDisk.Path = "/recordings"
	writeJSON(w, http.StatusOK, systemStats{
		Version:       emulatedVersion,
		Timestamp:     time.Now().UTC().Format("2006-01-02T15:04:05.000000"),
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
		Disks:         []diskStatsPayload{disk},
		CPU:           cpuStatsPayload{CoreCount: runtime.NumCPU()},
		Memory: memoryStatsPayload{
			TotalBytes: memory.Sys,
			UsedBytes:  memory.Alloc,
			FreeBytes:  memory.Sys - memory.Alloc,
		},
		RecordingsDirStats:         recordingsDisk,
		FFmpegAvailable:            true,
		ComskipAvailable:           false,
		CommercialDetectionEnabled: false,
	})
}

type systemStats struct {
	Version                    string             `json:"version"`
	Timestamp                  string             `json:"timestamp"`
	UptimeSeconds              int64              `json:"uptime_seconds"`
	Disks                      []diskStatsPayload `json:"disks"`
	CPU                        cpuStatsPayload    `json:"cpu"`
	Memory                     memoryStatsPayload `json:"memory"`
	RecordingsDirStats         diskStatsPayload   `json:"recordings_dir_stats"`
	FFmpegAvailable            bool               `json:"ffmpeg_available"`
	ComskipAvailable           bool               `json:"comskip_available"`
	CommercialDetectionEnabled bool               `json:"commercial_detection_enabled"`
}

type diskStatsPayload struct {
	Path         string  `json:"path"`
	TotalBytes   uint64  `json:"total_bytes"`
	UsedBytes    uint64  `json:"used_bytes"`
	FreeBytes    uint64  `json:"free_bytes"`
	UsagePercent float64 `json:"usage_percent"`
}

type cpuStatsPayload struct {
	UsagePercent      float64 `json:"usage_percent"`
	CoreCount         int     `json:"core_count"`
	LoadAvgOneMin     float64 `json:"load_avg_one_min"`
	LoadAvgFiveMin    float64 `json:"load_avg_five_min"`
	LoadAvgFifteenMin float64 `json:"load_avg_fifteen_min"`
}

type memoryStatsPayload struct {
	TotalBytes   uint64  `json:"total_bytes"`
	UsedBytes    uint64  `json:"used_bytes"`
	FreeBytes    uint64  `json:"free_bytes"`
	UsagePercent float64 `json:"usage_percent"`
}

func recordingResponse(recording storedRecording, entry map[string]any, now time.Time) map[string]any {
	request := recording.Request
	status := recording.StatusOverride
	if status == "" {
		status = tvhStatus(entry, request, now)
	}
	var filePath any
	if value := stringField(entry, "filename"); value != "" {
		filePath = value
	}
	return map[string]any{
		"name": request.Name, "url": request.URL, "start_time": request.StartTime,
		"duration_seconds": request.DurationSeconds, "description": request.Description,
		"parent_id": request.ParentID, "headers": request.Headers, "metadata": request.Metadata,
		"recurrence_days": nil, "recurrence_end_date": nil, "id": recording.ID,
		"status": status, "created_at": recording.CreatedAt, "file_path": filePath,
		"error": nil, "recovery_events": nil, "recurrence_group_id": nil, "recurrence_scheduled": nil,
	}
}

func tvhStatus(entry map[string]any, request recordingCreate, now time.Time) string {
	text := strings.ToLower(strings.Join([]string{stringField(entry, "status"), stringField(entry, "sched_status"), stringField(entry, "state")}, " "))
	switch {
	case strings.Contains(text, "failed"), strings.Contains(text, "missed"), strings.Contains(text, "invalid"), strings.Contains(text, "missing"):
		return "failed"
	case strings.Contains(text, "completed"), strings.Contains(text, "finished"):
		return "completed"
	case strings.Contains(text, "recording"):
		return "recording"
	case strings.Contains(text, "scheduled"), strings.Contains(text, "pending"):
		return "scheduled"
	}
	if entry == nil {
		if now.Before(request.StartTime.Add(time.Duration(request.DurationSeconds) * time.Second)) {
			return "scheduled"
		}
		return "failed"
	}
	if now.Before(request.StartTime) {
		return "scheduled"
	}
	if now.Before(request.StartTime.Add(time.Duration(request.DurationSeconds) * time.Second)) {
		return "recording"
	}
	return "completed"
}

func stringField(object map[string]any, key string) string {
	if value, ok := object[key].(string); ok {
		return value
	}
	return ""
}

func diskStats(path string) diskStatsPayload {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		_ = syscall.Statfs("/", &stat)
	}
	total := uint64(stat.Blocks) * uint64(stat.Bsize)
	free := uint64(stat.Bavail) * uint64(stat.Bsize)
	used := total - free
	percentage := float64(0)
	if total != 0 {
		percentage = float64(used) / float64(total) * 100
	}
	return diskStatsPayload{Path: path, TotalBytes: total, UsedBytes: used, FreeBytes: free, UsagePercent: percentage}
}

func randomUUID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	value := hex.EncodeToString(data)
	return fmt.Sprintf("%s-%s-%s-%s-%s", value[:8], value[8:12], value[12:16], value[16:20], value[20:]), nil
}

func randomToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return decoder.Decode(target)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}

func copyResponseHeaders(destination, source http.Header) {
	for key, values := range source {
		switch strings.ToLower(key) {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(writer, r)
		log.Printf("%s %s %d", r.Method, r.URL.RequestURI(), writer.status)
	})
}
