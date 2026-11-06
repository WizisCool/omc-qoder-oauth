package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// liveSender executes plugin HTTP requests against a local test server, so the
// whole signed request path is exercised over real HTTP.
type liveSender struct {
	client *http.Client
	base   *url.URL
}

func (s liveSender) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	u, err := url.Parse(req.URL)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	u.Scheme, u.Host = s.base.Scheme, s.base.Host
	httpReq, err := http.NewRequest(req.Method, u.String(), bytes.NewReader(req.Body))
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	for name, values := range req.Headers {
		for _, value := range values {
			httpReq.Header.Add(name, value)
		}
	}
	resp, err := s.client.Do(httpReq)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	return pluginapi.HTTPResponse{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), Body: body}, nil
}

func signedPath(path string) string {
	if strings.HasPrefix(path, "/algo") {
		return path[len("/algo"):]
	}
	return path
}

// requireCosy verifies the envelope signature the server would verify.
func requireCosy(t *testing.T, r *http.Request, body []byte) {
	t.Helper()
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer COSY.") {
		t.Fatalf("%s: unsigned request %q", r.URL.Path, auth)
	}
	rest := strings.TrimPrefix(auth, "Bearer COSY.")
	cut := strings.LastIndex(rest, ".")
	if cut < 0 {
		t.Fatalf("%s: malformed authorization", r.URL.Path)
	}
	payload, sig := rest[:cut], rest[cut+1:]
	key := r.Header.Get("Cosy-Key")
	date := r.Header.Get("Cosy-Date")
	sum := md5.Sum([]byte(payload + "\n" + key + "\n" + date + "\n" + string(body) + "\n" + signedPath(r.URL.Path)))
	if hex.EncodeToString(sum[:]) != sig {
		t.Fatalf("%s: signature does not cover the request", r.URL.Path)
	}
	if r.Header.Get("Cosy-Version") == "" || r.Header.Get("Cosy-User") == "" || r.Header.Get("Cosy-MachineOS") == "" {
		t.Fatalf("%s: envelope headers are incomplete", r.URL.Path)
	}
}

func TestSignedProtocolEndToEnd(t *testing.T) {
	t.Cleanup(func() { _ = configure(nil) })
	_ = configure(nil)

	polls := 0
	var chatBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/deviceToken/poll", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("challenge_method") != "S256" || r.URL.Query().Get("verifier") == "" {
			t.Errorf("poll query = %v", r.URL.Query())
		}
		polls++
		if polls == 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"token":"dt-live","refresh_token":"dr-live","user_id":"user-live"}`)
	})
	mux.HandleFunc("/api/v1/me/jobToken", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dt-live" {
			t.Errorf("job token auth = %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"token":"jt-live","refresh_token":"rt-live","expires_in":7200000}`)
	})
	mux.HandleFunc("/api/v1/userinfo", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"email":"live@example.test","name":"Live","id":"user-live"}`)
	})
	mux.HandleFunc("/algo/api/v2/model/list", func(w http.ResponseWriter, r *http.Request) {
		requireCosy(t, r, nil)
		_, _ = io.WriteString(w, `{"app":[{"key":"ultimate","display_name":"Ultimate","enable":true,"max_input_tokens":1000000,"thinking_config":{"disabled":{},"enabled":{"efforts":{"low":{},"high":{"is_default":true}}}}}]}`)
	})
	mux.HandleFunc("/algo/api/v2/service/pro/sse/agent_chat_generation", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		requireCosy(t, r, raw)
		if r.Header.Get("X-Model-Key") != "ultimate" || r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("chat headers = %v", r.Header)
		}
		plain, err := decodeBody(string(raw))
		if err != nil {
			t.Errorf("wire body: %v", err)
			return
		}
		if err := json.Unmarshal(plain, &chatBody); err != nil {
			t.Errorf("decoded body is not JSON: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseOf(`{"choices":[{"delta":{"content":"你好"}}]}`))
		_, _ = io.WriteString(w, sseOf(`{"choices":[{"delta":{"content":" world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	base, _ := url.Parse(server.URL)
	sender := liveSender{client: server.Client(), base: base}

	dir := t.TempDir()
	start, err := startLogin(pluginapi.AuthLoginStartRequest{Provider: siteQoder, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender)
	if err != nil {
		t.Fatal(err)
	}
	poll, err := pollLogin(pluginapi.AuthLoginPollRequest{Provider: siteQoder, State: start.State, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender)
	if err != nil || poll.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("first poll = %v, %v", poll, err)
	}
	poll, err = pollLogin(pluginapi.AuthLoginPollRequest{Provider: siteQoder, State: start.State, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender)
	if err != nil || poll.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("second poll = %v, %v", poll, err)
	}
	c, err := readCredential(poll.Auth.StorageJSON)
	if err != nil {
		t.Fatal(err)
	}
	if c.Access != "jt-live" || c.Refresh != "rt-live" || poll.Auth.Label != "live@example.test" {
		t.Fatalf("credential = %+v", c)
	}

	resp, err := execute(pluginapi.ExecutorRequest{
		Model:       "ultimate",
		Payload:     []byte(`{"model":"ultimate","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`),
		StorageJSON: poll.Auth.StorageJSON,
		AuthID:      poll.Auth.ID,
	}, sender)
	if err != nil {
		t.Fatal(err)
	}
	var answer struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp.Payload, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Choices[0].Message.Content != "你好 world" {
		t.Fatalf("answer = %s", resp.Payload)
	}
	if chatBody["session_id"] == "" || chatBody["request_id"] == "" {
		t.Fatalf("chat body = %v", chatBody)
	}
	cfg, _ := chatBody["model_config"].(map[string]any)
	if cfg["key"] != "ultimate" {
		t.Fatalf("model config = %v", cfg)
	}
}

func TestDevicePollDeclineEndsTheFlow(t *testing.T) {
	sender := &fakeSender{reply: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: 403, Body: []byte(`{}`)}, nil
	}}
	dir := t.TempDir()
	start, err := startLogin(pluginapi.AuthLoginStartRequest{Provider: siteQoder, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender)
	if err != nil {
		t.Fatal(err)
	}
	poll, err := pollLogin(pluginapi.AuthLoginPollRequest{Provider: siteQoder, State: start.State, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, sender)
	if err != nil || poll.Status != pluginapi.AuthLoginStatusError || !strings.Contains(poll.Message, "declined") {
		t.Fatalf("poll = %v, %v", poll, err)
	}
}

func TestExpiredLoginStateIsRejected(t *testing.T) {
	dir := t.TempDir()
	key, _ := loadStateKey(dir)
	nonce, _ := newNonce()
	machine, _ := newNonce()
	encoded, err := sealState(key, loginState{Site: siteQoder, Expires: time.Now().Add(-time.Minute).Unix(), Nonce: nonce, MachineID: machine})
	if err != nil {
		t.Fatal(err)
	}
	poll, err := pollLogin(pluginapi.AuthLoginPollRequest{Provider: siteQoder, State: encoded, Host: pluginapi.HostConfigSummary{AuthDir: dir}}, &fakeSender{})
	if err != nil || poll.Status != pluginapi.AuthLoginStatusError || !strings.Contains(poll.Message, "expired") {
		t.Fatalf("poll = %v, %v", poll, err)
	}
}
