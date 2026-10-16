package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLocalAPIKeyIsRequiredAndNeverForwarded(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Cookie"} {
			if _, exists := r.Header[http.CanonicalHeaderKey(name)]; exists {
				t.Errorf("upstream received credential header %s", name)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	endpoint, _ := url.Parse(upstream.URL)
	for _, path := range []string{"/v1/models", "/v1/chat/completions", "/v1/responses"} {
		for _, auth := range []string{"", "Bearer wrong-key", "Bearer client-secret"} {
			t.Run(path+"/"+auth, func(t *testing.T) {
				method := http.MethodPost
				if path == "/v1/models" {
					method = http.MethodGet
				}
				r := httptest.NewRequest(method, path, strings.NewReader(`{"model":"model","messages":[]}`))
				r.Header.Set("Authorization", auth)
				r.Header.Set("X-Api-Key", "client-secret")
				r.Header.Set("X-Goog-Api-Key", "client-secret")
				r.Header.Set("Cookie", "client-secret")
				before := requests.Load()
				w := httptest.NewRecorder()
				handler(endpoint, "client-secret", "1.19.1").ServeHTTP(w, r)
				if auth == "Bearer client-secret" {
					if w.Code != http.StatusOK || requests.Load() != before+1 {
						t.Fatalf("valid key failed: %d", w.Code)
					}
					return
				}
				if w.Code != http.StatusUnauthorized || requests.Load() != before {
					t.Fatalf("unauthorized request forwarded: %d", w.Code)
				}
			})
		}
	}
}

func TestFreeChatRoundTrip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zen/v1/chat/completions" || r.URL.Query().Get("trace") != "1" {
			t.Errorf("unexpected upstream URL: %s", r.URL)
		}
		for key, value := range map[string]string{
			"Authorization": "", "User-Agent": "opencode/1.19.1",
			"X-Opencode-Client": "cli", "X-Opencode-Project": "global",
			"Cookie": "", "X-Api-Key": "", "X-Real-IP": "",
		} {
			if r.Header.Get(key) != value {
				t.Errorf("%s = %q, want %q", key, r.Header.Get(key), value)
			}
		}
		if !strings.HasPrefix(r.Header.Get("X-Opencode-Session"), "ses_") || !strings.HasPrefix(r.Header.Get("X-Opencode-Request"), "msg_") {
			t.Error("missing generated request/session identity")
		}
		for _, name := range []string{"X-Opencode-Session", "X-Opencode-Request"} {
			id := r.Header.Get(name)
			if len(id) != 30 {
				t.Errorf("invalid client ID length: %s", id)
				continue
			}
			stamp, err := strconv.ParseUint(id[4:16], 16, 48)
			if name == "X-Opencode-Session" {
				stamp = ^stamp
			}
			if err != nil || stamp&0xfff != 1 {
				t.Errorf("invalid client timestamp: %s", id)
			}
		}
		var body struct {
			Stream bool   `json:"stream"`
			Choice string `json:"tool_choice"`
			Tools  []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
			Options struct {
				Usage bool `json:"include_usage"`
			} `json:"stream_options"`
			Temperature float64 `json:"temperature"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if !body.Stream || !body.Options.Usage || body.Choice != "none" || len(body.Tools) != 3 || body.Temperature != 0.7 {
			t.Errorf("unexpected adapted request: %+v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Opencode-Log-Id", "test-log")
		io.WriteString(w, "data: {\"id\":\"chat1\",\"model\":\"deepseek-v4.1-flash-free\",\"created\":123,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"think\"}}]}\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"O\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"K\"},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	endpoint, _ := url.Parse(upstream.URL + "/zen/v1")
	bridge := httptest.NewServer(handler(endpoint, "local-secret", "1.19.1"))
	defer bridge.Close()
	r, _ := http.NewRequest(http.MethodPost, bridge.URL+"/v1/chat/completions?trace=1", strings.NewReader(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"Hi"}],"temperature":0.7}`))
	r.Header.Set("Authorization", "Bearer local-secret")
	r.Header.Set("Cookie", "private-cookie")
	r.Header.Set("X-Api-Key", "private-key")
	r.Header.Set("X-Real-IP", "private-ip")
	response, err := bridge.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result struct {
		Object  string       `json:"object"`
		Choices []chatChoice `json:"choices"`
		Usage   struct {
			Total int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "application/json" || response.Header.Get("X-Opencode-Log-Id") != "test-log" {
		t.Fatalf("unexpected response: %s %v", response.Status, response.Header)
	}
	if result.Object != "chat.completion" || len(result.Choices) != 1 || result.Choices[0].Message.Content == nil || *result.Choices[0].Message.Content != "OK" || result.Choices[0].Message.Reasoning != "think" || result.Usage.Total != 5 {
		t.Fatalf("unexpected completion: %+v", result)
	}
}

func TestChatPreservesCallerToolsAndPaidBody(t *testing.T) {
	paid := `{"model":"paid-model","stream":false,"messages":[],"custom":{"value":1}}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(paid))
	collect, err := prepareChat(r)
	if err != nil || collect {
		t.Fatalf("prepare paid chat: %v, %v", collect, err)
	}
	body, _ := io.ReadAll(r.Body)
	if string(body) != paid {
		t.Fatalf("paid request changed: %s", body)
	}
	r = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"model-free","stream":true,"tools":[{"type":"function","function":{"name":"weather"}}],"tool_choice":"auto","custom":{"value":1}}`))
	collect, err = prepareChat(r)
	if err != nil || collect {
		t.Fatalf("prepare streaming chat: %v, %v", collect, err)
	}
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["tool_choice"]) != `"auto"` || string(fields["custom"]) != `{"value":1}` || !strings.Contains(string(fields["tools"]), `"weather"`) || strings.Contains(string(fields["tools"]), `"read"`) {
		t.Fatalf("caller options changed: %v", fields)
	}
}

func TestCollectToolCallsAndRejectIncompleteStream(t *testing.T) {
	stream := "data: {\"id\":\"chat1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call1\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"}错误\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"
	for _, done := range []bool{false, true} {
		body := stream
		if done {
			body += "data: [DONE]\n\n"
		}
		r := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
		err := collectChat(r)
		if !done {
			if err == nil {
				t.Fatal("accepted truncated SSE stream")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Choices []chatChoice `json:"choices"`
		}
		if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		tool := result.Choices[0].Message.Tools[0]
		if tool.ID != "call1" || tool.Function.Name != "read" || tool.Function.Arguments != "{}错误" {
			t.Fatalf("incorrect tool aggregation: %+v", tool)
		}
	}
}

func TestUpstreamErrorAndStreamingPassThrough(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := "data: {\"choices\":[]}\n\ndata: [DONE]\n\n"
			if status != http.StatusOK {
				body = `{"error":{"message":"upstream rejection"}}`
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Retry-After", "10")
				w.WriteHeader(status)
				io.WriteString(w, body)
			}))
			defer upstream.Close()
			endpoint, _ := url.Parse(upstream.URL)
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"model-free","stream":`+map[bool]string{true: "true", false: "false"}[status == 200]+`,"messages":[]}`))
			r.Header.Set("Authorization", "Bearer local-secret")
			w := httptest.NewRecorder()
			handler(endpoint, "local-secret", "1.19.1").ServeHTTP(w, r)
			if w.Code != status || w.Body.String() != body || w.Header().Get("Retry-After") != "10" {
				t.Fatalf("response changed: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
