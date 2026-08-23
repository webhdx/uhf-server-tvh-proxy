package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func testResponse(request *http.Request, status int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     headers,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func testServer(t *testing.T, transport roundTripFunc) (*server, http.Handler, string) {
	t.Helper()
	tvh, err := newTVHClient("http://tvheadend:9981", "recorder", "secret")
	if err != nil {
		t.Fatal(err)
	}
	tvh.client.Transport = transport
	server := newServer(config{}, tvh)
	handler := server.routes()

	request := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{
        "email":"user@example.com","password":"not-stored","device_id":"test-device"
    }`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("login returned %d: %s", response.Code, response.Body.String())
	}
	var token struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	return server, handler, token.IDToken
}

func authenticatedRequest(method, path, body, token string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func TestCreateAndListRecording(t *testing.T) {
	const tvhUUID = "0123456789abcdef0123456789abcdef"
	start := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	var createdConf map[string]any
	var cancelledUUID string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/dvr/entry/create":
			if err := request.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(request.Form.Get("conf")), &createdConf); err != nil {
				t.Fatal(err)
			}
			return testResponse(request, http.StatusOK, `{"uuid":"`+tvhUUID+`"}`, nil), nil
		case "/api/dvr/entry/grid":
			if request.URL.Query().Get("limit") != "100000" {
				t.Fatalf("unexpected grid query: %s", request.URL.RawQuery)
			}
			return testResponse(request, http.StatusOK, `{"entries":[{"uuid":"`+tvhUUID+`","title":"Evening News","start":`+strconv.FormatInt(start.Unix(), 10)+`,"stop":`+strconv.FormatInt(start.Add(30*time.Minute).Unix(), 10)+`,"create":`+strconv.FormatInt(start.Add(-time.Minute).Unix(), 10)+`,"sched_status":"scheduled"}]}`, nil), nil
		case "/api/dvr/entry/cancel":
			if err := request.ParseForm(); err != nil {
				t.Fatal(err)
			}
			cancelledUUID = request.Form.Get("uuid")
			return testResponse(request, http.StatusOK, `{}`, nil), nil
		default:
			t.Fatalf("unexpected TVHeadend request: %s", request.URL.String())
			return nil, nil
		}
	})
	_, handler, token := testServer(t, transport)
	body := `{"name":"Evening News","url":"http://tvheadend:9981/stream/channelid/42?profile=pass","start_time":"` + start.Format(time.RFC3339) + `","duration_seconds":1800,"description":"BBC One"}`

	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, authenticatedRequest(http.MethodPost, "/dvr/recordings", body, token))
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", createResponse.Code, createResponse.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(createResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if createdConf["channelname"] != "BBC One" {
		t.Fatalf("channelname = %#v", createdConf["channelname"])
	}
	if created["id"] != "01234567-89ab-cdef-0123-456789abcdef" {
		t.Fatalf("created id = %#v, want TVHeadend UUID", created["id"])
	}
	if createdConf["start"] != float64(start.Unix()) || createdConf["stop"] != float64(start.Add(30*time.Minute).Unix()) {
		t.Fatalf("unexpected timer range: %#v", createdConf)
	}

	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, authenticatedRequest(http.MethodGet, "/dvr/recordings", "", token))
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list returned %d: %s", listResponse.Code, listResponse.Body.String())
	}
	var recordings []map[string]any
	if err := json.Unmarshal(listResponse.Body.Bytes(), &recordings); err != nil {
		t.Fatal(err)
	}
	if len(recordings) != 1 || recordings[0]["status"] != "scheduled" || recordings[0]["name"] != "Evening News" {
		t.Fatalf("unexpected recordings: %#v", recordings)
	}

	cancelResponse := httptest.NewRecorder()
	handler.ServeHTTP(cancelResponse, authenticatedRequest(http.MethodPatch, "/dvr/recordings/"+created["id"].(string)+"/cancel", "", token))
	if cancelResponse.Code != http.StatusOK || cancelledUUID != tvhUUID {
		t.Fatalf("cancel returned %d, TVHeadend UUID %q: %s", cancelResponse.Code, cancelledUUID, cancelResponse.Body.String())
	}
	var cancelled map[string]any
	if err := json.Unmarshal(cancelResponse.Body.Bytes(), &cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled["status"] != "cancelled" {
		t.Fatalf("unexpected cancelled recording: %#v", cancelled)
	}
}

func TestTVHeadendRecordingGetsCompatibleIDAndURL(t *testing.T) {
	client, err := newTVHClient("http://tvh:9981/root", "", "")
	if err != nil {
		t.Fatal(err)
	}
	entry := map[string]any{
		"uuid": "0123456789abcdef0123456789abcdef", "channel": "abcdef0123456789abcdef0123456789",
		"title": "Imported", "start": float64(1), "stop": float64(61), "channelname": "Polsat Games HD",
	}
	entry["url"] = client.recordingURL(entry)
	response := recordingResponse(entry, time.Unix(2, 0))
	if response["id"] != "01234567-89ab-cdef-0123-456789abcdef" {
		t.Fatalf("unexpected id: %#v", response["id"])
	}
	if response["url"] != "http://tvh:9981/root/stream/channel/abcdef0123456789abcdef0123456789" {
		t.Fatalf("unexpected URL: %#v", response["url"])
	}
	if response["recurrence_scheduled"] != false {
		t.Fatalf("unexpected recurrence_scheduled: %#v", response["recurrence_scheduled"])
	}
	if response["description"] != "Polsat Games HD" {
		t.Fatalf("unexpected channel description: %#v", response["description"])
	}
}

func TestRecordingMetadataPreservesExistingValues(t *testing.T) {
	entry := map[string]any{
		"channelname": "TVHeadend Channel",
		"metadata":    map[string]any{"name": "UHF Channel", "categoryName": "Sports"},
	}
	metadata := recordingMetadata(entry)
	if metadata["name"] != "UHF Channel" || metadata["categoryName"] != "Sports" {
		t.Fatalf("existing metadata was not preserved: %#v", metadata)
	}
}

func TestTVHeadendRelativeRecordingURLIsReplaced(t *testing.T) {
	client, err := newTVHClient("http://tvh:9981/root", "", "")
	if err != nil {
		t.Fatal(err)
	}
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return testResponse(request, http.StatusOK, `{"entries":[{"uuid":"0123456789abcdef0123456789abcdef","url":"dvrfile/0123456789abcdef0123456789abcdef","channel":"abcdef0123456789abcdef0123456789","channelname":"Polsat Games HD","channel_icon":"imagecache/5152"}]}`, nil), nil
	})
	entries, err := client.recordings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := entries["0123456789abcdef0123456789abcdef"]["url"]; got != "http://tvh:9981/root/stream/channel/abcdef0123456789abcdef0123456789" {
		t.Fatalf("unexpected URL: %#v", got)
	}
	entry := entries["0123456789abcdef0123456789abcdef"]
	metadata, _ := entry["metadata"].(map[string]any)
	if metadata["name"] != "Polsat Games HD" || metadata["thumbnailURL"] != "http://tvh:9981/root/imagecache/5152" || metadata["categoryName"] != "Others" {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
	if entry["parent_id"] != entry["url"] {
		t.Fatalf("parent_id = %#v, want URL %#v", entry["parent_id"], entry["url"])
	}
}

func TestHLSRouteMatchesAuthenticationContract(t *testing.T) {
	const tvhUUID = "abcdef0123456789abcdef0123456789"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/dvr/entry/grid" {
			t.Fatalf("unexpected TVHeadend request: %s", request.URL.String())
		}
		return testResponse(request, http.StatusOK, `{"entries":[{"uuid":"`+tvhUUID+`","title":"Test","start":1,"stop":2}]}`, nil), nil
	})
	_, handler, token := testServer(t, transport)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/dvr/recordings/"+formatUUID(tvhUUID)+"/hls/index.m3u8?token="+token, nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("HLS route returned %d: %s", response.Code, response.Body.String())
	}
}

func TestMetadataUpdateReturnsCurrentRecordingWithoutPersistence(t *testing.T) {
	const tvhUUID = "abcdef0123456789abcdef0123456789"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/dvr/entry/grid" {
			t.Fatalf("unexpected TVHeadend request: %s", request.URL.String())
		}
		return testResponse(request, http.StatusOK, `{"entries":[{"uuid":"`+tvhUUID+`","title":"Test","start":1,"stop":2,"metadata":{"existing":true}}]}`, nil), nil
	})
	_, handler, token := testServer(t, transport)
	request := authenticatedRequest(http.MethodPatch, "/dvr/recordings/"+formatUUID(tvhUUID)+"/metadata", `{"metadata":{"new":"value"}}`, token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("metadata update returned %d: %s", response.Code, response.Body.String())
	}
	var recording map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &recording); err != nil {
		t.Fatal(err)
	}
	metadata, _ := recording["metadata"].(map[string]any)
	if metadata["existing"] != true || metadata["new"] != nil {
		t.Fatalf("metadata was unexpectedly changed: %#v", metadata)
	}
}

func TestChannelSelectorPrefersFullTVHUUID(t *testing.T) {
	description := "Wrong fallback"
	selector, err := channelFromRequest(recordingCreate{
		URL:         "http://tvheadend:9981/stream/channel/abcdef0123456789abcdef0123456789",
		Description: &description,
	})
	if err != nil {
		t.Fatal(err)
	}
	if selector.field != "channel" || selector.value != "abcdef0123456789abcdef0123456789" {
		t.Fatalf("unexpected selector: %#v", selector)
	}
}

func TestStreamForwardsRangeAndUsesTVHeadendCredentials(t *testing.T) {
	const tvhUUID = "abcdef0123456789abcdef0123456789"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/api/dvr/entry/grid" {
			return testResponse(request, http.StatusOK, `{"entries":[{"uuid":"`+tvhUUID+`","title":"Test","start":1,"stop":2}]}`, nil), nil
		}
		if request.URL.Path != "/dvrfile/"+tvhUUID {
			t.Fatalf("unexpected stream path: %s", request.URL.Path)
		}
		if request.Header.Get("Range") != "bytes=10-19" {
			t.Fatalf("range was not forwarded: %q", request.Header.Get("Range"))
		}
		username, password, ok := request.BasicAuth()
		if !ok || username != "recorder" || password != "secret" {
			t.Fatalf("unexpected TVHeadend auth: %q %q %v", username, password, ok)
		}
		headers := http.Header{"Content-Range": {"bytes 10-19/100"}, "Content-Type": {"video/mp2t"}}
		return testResponse(request, http.StatusPartialContent, "0123456789", headers), nil
	})
	_, handler, token := testServer(t, transport)

	request := authenticatedRequest(http.MethodGet, "/dvr/recordings/"+tvhUUID+"/stream", "", token)
	request.Header.Set("Range", "bytes=10-19")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent || response.Body.String() != "0123456789" {
		t.Fatalf("unexpected stream response: %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Range") != "bytes 10-19/100" {
		t.Fatalf("content-range was not copied: %q", response.Header().Get("Content-Range"))
	}
}

func TestTVHStatusMapping(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	stop := start.Add(2 * time.Hour)
	cases := []struct {
		entry map[string]any
		want  string
	}{
		{map[string]any{"status": "Recording"}, "recording"},
		{map[string]any{"status": "Completed OK"}, "completed"},
		{map[string]any{"status": "File missing"}, "failed"},
		{map[string]any{"sched_status": "scheduled"}, "scheduled"},
		{map[string]any{"status": "Scheduled for recording", "sched_status": "scheduled"}, "scheduled"},
	}
	for _, test := range cases {
		if got := tvhStatus(test.entry, start, stop, time.Now()); got != test.want {
			t.Errorf("tvhStatus(%v) = %q, want %q", test.entry, got, test.want)
		}
	}
}

func TestEndpointPreservesHTTPRootAndQuery(t *testing.T) {
	client, err := newTVHClient("http://tvh:9981/root", "", "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(client.endpoint("/api/dvr/entry/grid?limit=100000"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/root/api/dvr/entry/grid" || parsed.Query().Get("limit") != "100000" {
		t.Fatalf("unexpected endpoint: %s", parsed.String())
	}
}

func TestInvalidBearerMatchesUHFServer(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected TVHeadend request: %s", request.URL.String())
		return nil, nil
	})
	_, handler, _ := testServer(t, transport)
	request := authenticatedRequest(http.MethodGet, "/dvr/recordings", "", "invalid-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("unexpected authentication response: %d, WWW-Authenticate=%q", response.Code, response.Header().Get("WWW-Authenticate"))
	}
}

func TestTokenSurvivesAcrossServerInstances(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour)
	first := newServer(config{ServerPassword: "shared-secret"}, nil)
	second := newServer(config{ServerPassword: "shared-secret"}, nil)
	token := first.createToken(expiresAt)
	if !second.validToken(token) {
		t.Fatal("token was not valid on another server instance")
	}
	if newServer(config{ServerPassword: "other-secret"}, nil).validToken(token) {
		t.Fatal("token was valid with a different server password")
	}
}

func TestStatsMatchesObservedUHFShape(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/serverinfo" {
			t.Fatalf("unexpected TVHeadend request: %s", request.URL.String())
		}
		return testResponse(request, http.StatusOK, `{"name":"Tvheadend"}`, nil), nil
	})
	_, handler, _ := testServer(t, transport)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/server/stats", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("stats returned %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if strings.Contains(body, "state.json") {
		t.Fatalf("stats leaked proxy state path: %s", body)
	}
	if !strings.Contains(body, `"recordings_dir_stats":{"path":"/recordings"`) {
		t.Fatalf("stats did not emulate the UHF recordings path: %s", body)
	}
	if strings.Index(body, `"version"`) > strings.Index(body, `"timestamp"`) || strings.Index(body, `"timestamp"`) > strings.Index(body, `"uptime_seconds"`) {
		t.Fatalf("top-level fields do not follow the observed UHF order: %s", body)
	}
}
