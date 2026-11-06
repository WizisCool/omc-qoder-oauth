package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type httpSender interface {
	Do(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

type cpaSender struct{ callbackID string }

func (s cpaSender) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	var out pluginapi.HTTPResponse
	err := hostRPC("host.http.do", map[string]any{"host_callback_id": s.callbackID, "request": req}, &out)
	if err != nil {
		return out, fmt.Errorf("upstream transport failed")
	}
	return out, nil
}

// apiError is an OpenAPI failure on qoder.com/qoder.cn, which answers JSON.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("upstream error HTTP %d code %s", e.Status, e.Code)
	}
	return fmt.Sprintf("upstream error HTTP %d", e.Status)
}

type apiEnvelope struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
	Msg     string          `json:"msg"`
	Data    json.RawMessage `json:"data"`
}

func (e apiEnvelope) message() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Msg
}

func (e apiEnvelope) code() string {
	if len(e.Code) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(e.Code, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(e.Code, &n) == nil {
		return n.String()
	}
	return ""
}

// openAPI calls one of Qoder's account endpoints, whose JSON envelopes carry a
// code and (for the job token exchange) a data object. A 2xx with a non-zero
// code is a failure, not a result.
func openAPI(sender httpSender, method, target, token string, body any) (json.RawMessage, error) {
	h := http.Header{"Accept": {"application/json"}, "User-Agent": {"Qoder"}}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
		h.Set("Content-Type", "application/json")
	}
	resp, err := sender.Do(pluginapi.HTTPRequest{Method: method, URL: target, Headers: h, Body: payload})
	if err != nil {
		return nil, err
	}
	var env apiEnvelope
	if json.Unmarshal(resp.Body, &env) != nil {
		return nil, &apiError{Status: resp.StatusCode, Message: "non-JSON upstream response"}
	}
	code := env.code()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || (code != "" && code != "0" && code != "200") {
		return nil, &apiError{Status: resp.StatusCode, Code: code, Message: env.message()}
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return resp.Body, nil
	}
	return env.Data, nil
}

// statusOf is the upstream HTTP status of an error, 0 when there was no response.
func statusOf(err error) int {
	var e *apiError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// pluginabiError is a host-facing error with an HTTP status.
func pluginabiError(code, message string, status int) error {
	return pluginabi.NewError(code, message, status)
}
