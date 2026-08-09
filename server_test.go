package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	store, err := openStore(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatal(err)
	}
	server := newServer(config{StatePath: t.TempDir() + "/state.json"}, tvh, store)
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
			return testResponse(request, http.StatusOK, `{"entries":[{"uuid":"`+tvhUUID+`","sched_status":"scheduled"}]}`, nil), nil
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
	start := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
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
	server, handler, token := testServer(t, transport)
	stored := storedRecording{
		ID: "8ed620c7-3f0b-4b7f-8f6c-3e4106960cbc", TVHUUID: tvhUUID, CreatedAt: time.Now(),
		Request: recordingCreate{Name: "Test", URL: "http://tvh/stream", StartTime: time.Now(), DurationSeconds: 60},
	}
	if err := server.store.put(stored); err != nil {
		t.Fatal(err)
	}

	request := authenticatedRequest(http.MethodGet, "/dvr/recordings/"+stored.ID+"/stream", "", token)
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
	request := recordingCreate{StartTime: time.Now().Add(-time.Hour), DurationSeconds: 7200}
	cases := []struct {
		entry map[string]any
		want  string
	}{
		{map[string]any{"status": "Recording"}, "recording"},
		{map[string]any{"status": "Completed OK"}, "completed"},
		{map[string]any{"status": "File missing"}, "failed"},
		{map[string]any{"sched_status": "scheduled"}, "scheduled"},
	}
	for _, test := range cases {
		if got := tvhStatus(test.entry, request, time.Now()); got != test.want {
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
