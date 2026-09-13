package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type tvhClient struct {
	baseURL  *url.URL
	username string
	password string
	client   *http.Client
}

func newTVHClient(rawURL, username, password string) (*tvhClient, error) {
	baseURL, err := url.Parse(rawURL)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, fmt.Errorf("invalid TVH_URL %q", rawURL)
	}
	return &tvhClient{
		baseURL:  baseURL,
		username: username,
		password: password,
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 15 * time.Second,
		}},
	}, nil
}

func (c *tvhClient) endpoint(path string) string {
	result := *c.baseURL
	requestPath, query, _ := strings.Cut(path, "?")
	result.Path = strings.TrimRight(result.Path, "/") + requestPath
	result.RawQuery = query
	return result.String()
}

func (c *tvhClient) api(ctx context.Context, path string, values url.Values, target any) error {
	var request *http.Request
	var err error
	if values == nil {
		request, err = http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/api/"+path), nil)
	} else {
		request, err = http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/api/"+path), strings.NewReader(values.Encode()))
		if err == nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	if err != nil {
		return err
	}
	c.authenticate(request)
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("TVHeadend request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read TVHeadend response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("TVHeadend %s returned %s: %s", path, response.Status, strings.TrimSpace(string(body)))
	}
	if target != nil && len(body) != 0 {
		if err := json.Unmarshal(body, target); err != nil {
			return fmt.Errorf("decode TVHeadend %s response: %w", path, err)
		}
	}
	return nil
}

func (c *tvhClient) authenticate(request *http.Request) {
	if c.username != "" {
		request.SetBasicAuth(c.username, c.password)
	}
}

func (c *tvhClient) ping(ctx context.Context) error {
	var result map[string]any
	return c.api(ctx, "serverinfo", nil, &result)
}

type tvhDiskStats struct {
	Free  uint64
	Used  uint64
	Total uint64
}

func (c *tvhClient) diskStats(ctx context.Context) (tvhDiskStats, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/comet/poll?immediate=1"), nil)
	if err != nil {
		return tvhDiskStats{}, err
	}
	c.authenticate(request)
	response, err := c.client.Do(request)
	if err != nil {
		return tvhDiskStats{}, fmt.Errorf("TVHeadend disk stats request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return tvhDiskStats{}, fmt.Errorf("read TVHeadend disk stats response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return tvhDiskStats{}, fmt.Errorf("TVHeadend comet/poll returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var result struct {
		Messages []struct {
			NotificationClass string `json:"notificationClass"`
			Free              int64  `json:"freediskspace"`
			Used              int64  `json:"useddiskspace"`
			Total             int64  `json:"totaldiskspace"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return tvhDiskStats{}, fmt.Errorf("decode TVHeadend disk stats response: %w", err)
	}
	for _, message := range result.Messages {
		if message.NotificationClass == "accessUpdate" && message.Free >= 0 && message.Used >= 0 && message.Total > 0 {
			return tvhDiskStats{Free: uint64(message.Free), Used: uint64(message.Used), Total: uint64(message.Total)}, nil
		}
	}
	return tvhDiskStats{}, fmt.Errorf("TVHeadend accessUpdate did not contain disk stats")
}

func (c *tvhClient) createRecording(ctx context.Context, request recordingCreate) (string, error) {
	channel, err := channelFromRequest(request)
	if err != nil {
		return "", err
	}
	conf := map[string]any{
		"enabled": true,
		"start":   request.StartTime.Unix(),
		"stop":    request.StartTime.Add(time.Duration(request.DurationSeconds) * time.Second).Unix(),
		"title":   map[string]string{"eng": request.Name},
		"comment": "Created by uhf-server-tvh-proxy",
	}
	conf[channel.field] = channel.value
	encoded, err := json.Marshal(conf)
	if err != nil {
		return "", err
	}
	var result json.RawMessage
	if err := c.api(ctx, "dvr/entry/create", url.Values{"conf": {string(encoded)}}, &result); err != nil {
		return "", err
	}
	return parseCreatedUUID(result)
}

type channelSelector struct {
	field string
	value string
}

func channelFromRequest(request recordingCreate) (channelSelector, error) {
	streamURL, err := url.Parse(request.URL)
	if err == nil {
		parts := strings.Split(strings.Trim(streamURL.Path, "/"), "/")
		for index := 0; index+2 < len(parts); index++ {
			if parts[index] != "stream" {
				continue
			}
			switch parts[index+1] {
			case "channel":
				if isTVHUUID(parts[index+2]) {
					return channelSelector{field: "channel", value: parts[index+2]}, nil
				}
			case "channelname":
				return channelSelector{field: "channelname", value: parts[index+2]}, nil
			}
		}
	}
	if request.Description != nil && strings.TrimSpace(*request.Description) != "" {
		return channelSelector{field: "channelname", value: strings.TrimSpace(*request.Description)}, nil
	}
	return channelSelector{}, fmt.Errorf("cannot identify TVHeadend channel: use a /stream/channel/<uuid> URL or provide the channel name in description")
}

func parseCreatedUUID(data json.RawMessage) (string, error) {
	var object map[string]any
	if err := json.Unmarshal(data, &object); err == nil {
		for _, key := range []string{"uuid", "id"} {
			if value, ok := object[key].(string); ok && isTVHUUID(value) {
				return strings.ReplaceAll(value, "-", ""), nil
			}
		}
	}
	var value string
	if err := json.Unmarshal(data, &value); err == nil && isTVHUUID(value) {
		return strings.ReplaceAll(value, "-", ""), nil
	}
	return "", fmt.Errorf("TVHeadend create response did not contain a recording UUID: %s", string(data))
}

func isTVHUUID(value string) bool {
	compact := strings.ReplaceAll(value, "-", "")
	if len(compact) != 32 {
		return false
	}
	_, err := strconv.ParseUint(compact[:16], 16, 64)
	if err != nil {
		return false
	}
	_, err = strconv.ParseUint(compact[16:], 16, 64)
	return err == nil
}

func (c *tvhClient) recordings(ctx context.Context) (map[string]map[string]any, error) {
	var result struct {
		Entries []map[string]any `json:"entries"`
	}
	path := "dvr/entry/grid?limit=100000"
	if err := c.api(ctx, path, nil, &result); err != nil {
		return nil, err
	}
	entries := make(map[string]map[string]any, len(result.Entries))
	for _, entry := range result.Entries {
		if uuid, ok := entry["uuid"].(string); ok {
			if !isAbsoluteHTTPURL(stringField(entry, "url")) {
				entry["url"] = c.recordingURL(entry)
			}
			c.addUHFMetadata(entry)
			entries[compactUUID(uuid)] = entry
		}
	}
	return entries, nil
}

func (c *tvhClient) addUHFMetadata(entry map[string]any) {
	if _, exists := entry["metadata"].(map[string]any); exists {
		return
	}
	channelName := stringField(entry, "channelname")
	channelUUID := compactUUID(stringField(entry, "channel"))
	if channelName == "" || !isTVHUUID(channelUUID) {
		return
	}
	metadata := map[string]any{
		"name":                 channelName,
		"categoryName":         "Others",
		"originalCategoryName": "Others",
		"playlistID":           strings.ToUpper(formatUUID(channelUUID)),
	}
	if icon := strings.TrimLeft(stringField(entry, "channel_icon"), "/"); icon != "" {
		metadata["thumbnailURL"] = c.endpoint("/" + icon)
	}
	entry["metadata"] = metadata
	entry["parent_id"] = stringField(entry, "url")
}

func isAbsoluteHTTPURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func (c *tvhClient) recordingURL(entry map[string]any) string {
	if channel := stringField(entry, "channel"); isTVHUUID(channel) {
		return c.endpoint("/stream/channel/" + compactUUID(channel))
	}
	if channelName := stringField(entry, "channelname"); channelName != "" {
		return c.endpoint("/stream/channelname/" + url.PathEscape(channelName))
	}
	return c.endpoint("/dvrfile/" + compactUUID(stringField(entry, "uuid")))
}

func (c *tvhClient) cancel(ctx context.Context, uuid string) error {
	return c.api(ctx, "dvr/entry/cancel", url.Values{"uuid": {uuid}}, nil)
}

func (c *tvhClient) remove(ctx context.Context, uuid string) error {
	return c.api(ctx, "dvr/entry/remove", url.Values{"uuid": {uuid}}, nil)
}

func (c *tvhClient) stream(ctx context.Context, uuid string, source *http.Request) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/dvrfile/"+uuid), nil)
	if err != nil {
		return nil, err
	}
	for _, header := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		if value := source.Header.Get(header); value != "" {
			request.Header.Set(header, value)
		}
	}
	c.authenticate(request)
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("TVHeadend stream request: %w", err)
	}
	return response, nil
}
