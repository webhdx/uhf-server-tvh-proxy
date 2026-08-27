package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const emulatedVersion = "2.0.0"

type config struct {
	ListenAddr     string
	TVHURL         string
	TVHUsername    string
	TVHPassword    string
	ServerPassword string
}

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

type server struct {
	cfg       config
	tvh       *tvhClient
	startedAt time.Time
}

func newServer(cfg config, tvh *tvhClient) *server {
	return &server{cfg: cfg, tvh: tvh, startedAt: time.Now()}
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
	mux.HandleFunc("GET /dvr/recordings/{id}/hls/{name}", s.hlsAsset)
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
	expiresAt := time.Now().Add(24 * time.Hour)
	token := s.createToken(expiresAt)
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
	expiryText, signature, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	expected := s.signToken(expiryText)
	provided, err := hex.DecodeString(signature)
	return err == nil && hmac.Equal(provided, expected)
}

func (s *server) createToken(expiresAt time.Time) string {
	expiry := strconv.FormatInt(expiresAt.Unix(), 10)
	return expiry + "." + hex.EncodeToString(s.signToken(expiry))
}

func (s *server) signToken(expiry string) []byte {
	secret := s.cfg.ServerPassword
	if secret == "" {
		secret = "uhf-server-tvh-proxy"
	}
	signature := hmac.New(sha256.New, []byte(secret))
	_, _ = signature.Write([]byte(expiry))
	return signature.Sum(nil)
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
	writeJSON(w, http.StatusCreated, createdRecordingResponse(tvhUUID, request, now))
}

func (s *server) listRecordings(w http.ResponseWriter, r *http.Request) {
	entries, err := s.tvh.recordings(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	now := time.Now().UTC()
	result := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		result = append(result, recordingResponse(entry, now))
	}
	sort.Slice(result, func(i, j int) bool {
		return timeField(result[i], "start_time").Before(timeField(result[j], "start_time"))
	})
	writeJSON(w, http.StatusOK, result)
}

func (s *server) getRecording(w http.ResponseWriter, r *http.Request) {
	entry, err := s.recording(r)
	if err != nil {
		writeRecordingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, recordingResponse(entry, time.Now().UTC()))
}

func (s *server) cancelRecording(w http.ResponseWriter, r *http.Request) {
	entry, err := s.recording(r)
	if err != nil {
		writeRecordingError(w, err)
		return
	}
	uuid := compactUUID(r.PathValue("id"))
	if err := s.tvh.cancel(r.Context(), uuid); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	entry["sched_status"] = "cancelled"
	writeJSON(w, http.StatusOK, recordingResponse(entry, time.Now().UTC()))
}

func (s *server) deleteRecording(w http.ResponseWriter, r *http.Request) {
	entry, err := s.recording(r)
	if err != nil {
		writeRecordingError(w, err)
		return
	}
	response := recordingResponse(entry, time.Now().UTC())
	status, _ := response["status"].(string)
	uuid := compactUUID(r.PathValue("id"))
	if status == "completed" || status == "failed" {
		err = s.tvh.remove(r.Context(), uuid)
	} else if status != "cancelled" {
		err = s.tvh.cancel(r.Context(), uuid)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) getMetadata(w http.ResponseWriter, r *http.Request) {
	entry, err := s.recording(r)
	if err != nil {
		writeRecordingError(w, err)
		return
	}
	var metadata any
	if value := recordingMetadata(entry); value != nil {
		metadata = value
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	writeJSON(w, http.StatusOK, metadata)
}

func recordingMetadata(entry map[string]any) map[string]any {
	if existing, ok := entry["metadata"].(map[string]any); ok {
		metadata := make(map[string]any, len(existing))
		for key, value := range existing {
			metadata[key] = value
		}
		return metadata
	}
	return nil
}

func (s *server) updateMetadata(w http.ResponseWriter, r *http.Request) {
	var update struct {
		Metadata map[string]any `json:"metadata"`
	}
	if err := decodeJSON(r, &update); err != nil || update.Metadata == nil {
		writeError(w, http.StatusUnprocessableEntity, "metadata is required")
		return
	}
	entry, err := s.recording(r)
	if err != nil {
		writeRecordingError(w, err)
		return
	}
	log.Printf("metadata update ignored for recording %s: TVHeadend metadata persistence is unavailable", formatUUID(r.PathValue("id")))
	writeJSON(w, http.StatusOK, recordingResponse(entry, time.Now().UTC()))
}

func (s *server) streamRecording(w http.ResponseWriter, r *http.Request) {
	if _, err := s.recording(r); err != nil {
		writeRecordingError(w, err)
		return
	}
	response, err := s.tvh.stream(r.Context(), compactUUID(r.PathValue("id")), r)
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

func (s *server) hlsAsset(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateFromHeaderOrQuery(w, r) {
		return
	}
	if _, err := s.recording(r); err != nil {
		writeRecordingError(w, err)
		return
	}
	writeError(w, http.StatusNotFound, "HLS asset not available")
}

func (s *server) authenticateFromHeaderOrQuery(w http.ResponseWriter, r *http.Request) bool {
	if token := r.URL.Query().Get("token"); token != "" {
		if s.validToken(token) {
			return true
		}
		writeError(w, http.StatusForbidden, "Invalid or expired token")
		return false
	}
	authorized := false
	s.auth(func(http.ResponseWriter, *http.Request) { authorized = true })(w, r)
	return authorized
}

func (s *server) commercials(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"commercials": []any{}, "total_segments": 0})
}

func (s *server) cancelRecurrence(w http.ResponseWriter, r *http.Request) {
	if _, err := s.recording(r); err != nil {
		writeRecordingError(w, err)
		return
	}
	writeError(w, http.StatusBadRequest, "Recording does not have a recurrence schedule")
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

func createdRecordingResponse(uuid string, request recordingCreate, now time.Time) map[string]any {
	entry := map[string]any{
		"uuid": uuid, "title": request.Name, "start": request.StartTime.Unix(),
		"stop":   request.StartTime.Add(time.Duration(request.DurationSeconds) * time.Second).Unix(),
		"create": now.Unix(), "sched_status": "scheduled", "url": request.URL, "metadata": request.Metadata,
	}
	response := recordingResponse(entry, now)
	response["description"] = request.Description
	response["parent_id"] = request.ParentID
	response["headers"] = request.Headers
	return response
}

func recordingResponse(entry map[string]any, now time.Time) map[string]any {
	start := unixTimeField(entry, "start")
	stop := unixTimeField(entry, "stop")
	created := unixTimeField(entry, "create")
	if created.IsZero() {
		created = start
	}
	description := stringField(entry, "channelname")
	if description == "" {
		description = localizedStringField(entry, "description")
	}
	var descriptionValue any
	if description != "" {
		descriptionValue = description
	}
	var metadata any
	if value := recordingMetadata(entry); value != nil {
		metadata = value
	}
	var filePath any
	if value := stringField(entry, "filename"); value != "" {
		filePath = value
	}
	return map[string]any{
		"name": localizedStringField(entry, "disp_title", "title"), "url": stringField(entry, "url"), "start_time": start,
		"duration_seconds": int64(stop.Sub(start).Seconds()), "description": descriptionValue,
		"parent_id": nullableStringField(entry, "parent_id"), "headers": nil, "metadata": metadata,
		"recurrence_days": nil, "recurrence_end_date": nil, "id": formatUUID(stringField(entry, "uuid")),
		"status": tvhStatus(entry, start, stop, now), "created_at": created, "file_path": filePath,
		"error": nil, "recovery_events": nil, "recurrence_group_id": nil, "recurrence_scheduled": false,
	}
}

func nullableStringField(object map[string]any, key string) any {
	if value := stringField(object, key); value != "" {
		return value
	}
	return nil
}

func tvhStatus(entry map[string]any, start, stop, now time.Time) string {
	text := strings.ToLower(strings.Join([]string{stringField(entry, "status"), stringField(entry, "sched_status"), stringField(entry, "state")}, " "))
	switch {
	case strings.Contains(text, "cancel"):
		return "cancelled"
	case strings.Contains(text, "failed"), strings.Contains(text, "missed"), strings.Contains(text, "invalid"), strings.Contains(text, "missing"):
		return "failed"
	case strings.Contains(text, "completed"), strings.Contains(text, "finished"):
		return "completed"
	case strings.Contains(text, "scheduled"), strings.Contains(text, "pending"):
		return "scheduled"
	case strings.Contains(text, "recording"):
		return "recording"
	}
	if now.Before(start) {
		return "scheduled"
	}
	if now.Before(stop) {
		return "recording"
	}
	return "completed"
}

func localizedStringField(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringField(object, key); value != "" {
			return value
		}
		if values, ok := object[key].(map[string]any); ok {
			for _, value := range values {
				if text, ok := value.(string); ok && text != "" {
					return text
				}
			}
		}
	}
	return ""
}

func unixTimeField(object map[string]any, key string) time.Time {
	var seconds int64
	switch value := object[key].(type) {
	case float64:
		seconds = int64(value)
	case int64:
		seconds = value
	case json.Number:
		seconds, _ = value.Int64()
	}
	if seconds == 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

func timeField(object map[string]any, key string) time.Time {
	value, _ := object[key].(time.Time)
	return value
}

var errRecordingNotFound = errors.New("recording not found")

func (s *server) recording(r *http.Request) (map[string]any, error) {
	uuid := compactUUID(r.PathValue("id"))
	if !isTVHUUID(uuid) {
		return nil, errRecordingNotFound
	}
	entries, err := s.tvh.recordings(r.Context())
	if err != nil {
		return nil, err
	}
	entry, ok := entries[uuid]
	if !ok {
		return nil, errRecordingNotFound
	}
	return entry, nil
}

func writeRecordingError(w http.ResponseWriter, err error) {
	if errors.Is(err, errRecordingNotFound) {
		writeError(w, http.StatusNotFound, "Recording not found")
		return
	}
	writeError(w, http.StatusBadGateway, err.Error())
}

func compactUUID(value string) string {
	return strings.ReplaceAll(value, "-", "")
}

func formatUUID(value string) string {
	value = compactUUID(value)
	if !isTVHUUID(value) {
		return value
	}
	return value[:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:]
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
