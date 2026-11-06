package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Qoder's own failure codes. Only 105 marks a lapsed sign-in; a 401 or 403
// carrying another code is something else (a queued request, a refused input).
const (
	queuedCode = "10605"
)

var quotaCodes = map[string]bool{"110": true, "112": true, "113": true, "114": true, "115": true, "116": true, "117": true, "118": true, "119": true, "122": true}
var requestCodes = map[string]bool{"406": true, "416": true, "430": true, "80411": true, "80412": true, "100400": true, "100401": true, "100403": true}

// qoderFailure is a normalized upstream refusal.
type qoderFailure struct {
	Status     int
	Message    string
	Code       string
	RetryAfter int
	Lapsed     bool
	Queue      bool
	Nameless   bool
}

func decisionError(f *qoderFailure) error {
	if f == nil {
		return nil
	}
	status := f.Status
	if status == 0 {
		status = 502
	}
	err := pluginabi.NewError(failureCode(f), f.Message, status)
	if f.Code != "" {
		err = pluginabi.NewError(failureCode(f), f.Message, status)
	}
	err.RetryAfterSeconds = f.RetryAfter
	return err
}

func failureCode(f *qoderFailure) string {
	switch {
	case f.Lapsed:
		return "reauth_required"
	case f.Queue:
		return "upstream_busy"
	case f.Status == 429:
		return "quota_exhausted"
	case f.Status >= 400 && f.Status < 500:
		return "invalid_request"
	case f.Status >= 500:
		return "upstream_error"
	default:
		return "upstream_error"
	}
}

// classification bodies: Qoder's code says what a failure is.
type failureBody struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
	Queue   *struct {
		IsQueued          *bool    `json:"isQueued"`
		ServiceAvailable  *bool    `json:"serviceAvailable"`
		RetryAfterSeconds *float64 `json:"retryAfterSeconds"`
		QueueCount        *float64 `json:"queueCount"`
		QueueType         string   `json:"queueType"`
		ModelKey          string   `json:"modelKey"`
	} `json:"queue"`
}

func rawCode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// classifyUpstream maps an HTTP failure or an error frame to a normalized
// failure with the status the client should see.
func classifyUpstream(status int, body []byte) *qoderFailure {
	if status < 400 || status > 599 {
		status = 502
	}
	text := strings.TrimSpace(string(body))
	message := text
	code := ""
	var parsed failureBody
	if json.Unmarshal(body, &parsed) == nil {
		if parsed.Message != "" {
			message = parsed.Message
		}
		code = rawCode(parsed.Code)
		if len(parsed.Details) > 0 {
			var details struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(parsed.Details, &details) == nil && details.Error.Message != "" {
				message += ": " + details.Error.Message
			}
		}
		if parsed.Queue != nil && (parsed.Queue.IsQueued != nil && *parsed.Queue.IsQueued || parsed.Queue.ServiceAvailable != nil && !*parsed.Queue.ServiceAvailable) {
			wait := 30
			if parsed.Queue.RetryAfterSeconds != nil && *parsed.Queue.RetryAfterSeconds > 0 {
				wait = int(*parsed.Queue.RetryAfterSeconds)
			}
			what := "is busy"
			if parsed.Queue.IsQueued != nil && *parsed.Queue.IsQueued {
				what = "queued the request"
			}
			model := ""
			if parsed.Queue.ModelKey != "" {
				model = " for " + parsed.Queue.ModelKey
			}
			return &qoderFailure{Status: 503, Code: queuedCode, Message: fmt.Sprintf("Qoder %s%s; try again in %ds", what, model, wait), RetryAfter: wait, Queue: true}
		}
	} else {
		var env struct {
			StatusCodeValue *int   `json:"statusCodeValue"`
			Body            string `json:"body"`
		}
		if json.Unmarshal(body, &env) == nil && env.Body != "" {
			message = strings.TrimSpace(env.Body)
			if code == "" {
				var inner failureBody
				if json.Unmarshal([]byte(env.Body), &inner) == nil {
					code = rawCode(inner.Code)
					if inner.Message != "" {
						message = inner.Message
					}
				}
			}
		}
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if code == queuedCode {
		return &qoderFailure{Status: 503, Code: code, Message: "Qoder queued the request; try again shortly", RetryAfter: 30, Queue: true}
	}
	if quotaCodes[code] {
		return &qoderFailure{Status: 429, Code: code, Message: "Qoder usage limit reached: " + message}
	}
	if requestCodes[code] {
		return &qoderFailure{Status: 400, Code: code, Message: fmt.Sprintf("Qoder refused the request (%s): %s", code, message)}
	}
	if strings.Contains(strings.ToLower(message), "error in upstream response") {
		return &qoderFailure{Status: 502, Code: code, Message: message}
	}
	switch {
	case code == "105":
		return &qoderFailure{Status: 401, Code: code, Message: "Qoder sign-in has expired; sign in again", Lapsed: true}
	case status == 401 || status == 403:
		return &qoderFailure{Status: 401, Code: code, Message: "Qoder sign-in has expired; sign in again", Lapsed: true}
	case status == 429 || strings.Contains(strings.ToLower(message), "quota"):
		return &qoderFailure{Status: 429, Code: code, Message: "Qoder usage limit reached: " + message}
	}
	return &qoderFailure{Status: status, Code: code, Message: message}
}

// qoderToolCall is a tool call the answer asked for. Its name and id are known
// on the first event; later ones carry argument text for the same index.
type qoderToolCall struct {
	Index int
	ID    string
	Name  string
	Args  string
}

type qoderToolArgs struct {
	Index int
	Text  string
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"-"`
	ReasoningTokens  int `json:"-"`
}

// qoderEvent is one normalized piece of Qoder's reply.
type qoderEvent struct {
	Text      string
	Reasoning string
	Tool      *qoderToolCall
	Args      *qoderToolArgs
	Stop      string
	Usage     *openAIUsage
	Err       *qoderFailure
}

// sseDecoder turns arbitrary stream chunks into complete `data:` payload lines.
type sseDecoder struct {
	pending []byte
}

func (d *sseDecoder) feed(chunk []byte) []string {
	d.pending = append(d.pending, chunk...)
	var out []string
	for {
		i := bytes.IndexByte(d.pending, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(d.pending[:i]))
		d.pending = d.pending[i+1:]
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func (d *sseDecoder) flush() []string {
	line := strings.TrimSpace(string(d.pending))
	d.pending = nil
	if line == "" {
		return nil
	}
	return []string{line}
}

// qoderEvents reads Qoder's SSE frames and yields normalized events.
type qoderEvents struct {
	dec      sseDecoder
	tools    []chatTool
	native   map[int]*nativeCall
	index    int
	finish   string
	usage    *openAIUsage
	sawTool  bool
	finished bool
}

// nativeCall is one tool call Qoder streams the OpenAI way. Its index is the
// client-facing one, assigned when the model first names the call.
type nativeCall struct {
	ID    string
	Name  string
	Args  string
	Index int
	Named bool
}

func newQoderEvents(tools []chatTool) *qoderEvents {
	return &qoderEvents{tools: tools, native: map[int]*nativeCall{}, index: -1}
}

func (q *qoderEvents) feed(chunk []byte) []qoderEvent {
	var out []qoderEvent
	for _, line := range q.dec.feed(chunk) {
		out = append(out, q.line(line)...)
	}
	return out
}

func (q *qoderEvents) flush() []qoderEvent {
	var out []qoderEvent
	for _, line := range q.dec.flush() {
		out = append(out, q.line(line)...)
	}
	if q.finished {
		return out
	}
	stop := "stop"
	if q.sawTool {
		stop = "tool_calls"
	} else if q.finish == "length" {
		stop = "length"
	}
	out = append(out, qoderEvent{Stop: stop, Usage: q.usage})
	q.finished = true
	return out
}

type sseFrame struct {
	StatusCodeValue *int   `json:"statusCodeValue"`
	Body            string `json:"body"`
}

type streamDelta struct {
	Content          *string          `json:"content"`
	ReasoningContent string           `json:"reasoning_content"`
	ToolCalls        []streamToolCall `json:"tool_calls"`
}

type streamToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type streamChunk struct {
	Choices []struct {
		Delta        streamDelta `json:"delta"`
		FinishReason *string     `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error   *failureBody    `json:"error"`
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

func (q *qoderEvents) line(line string) []qoderEvent {
	if !strings.HasPrefix(line, "data:") {
		return nil
	}
	payload := strings.TrimSpace(line[len("data:"):])
	if payload == "" {
		return nil
	}
	var frame sseFrame
	if json.Unmarshal([]byte(payload), &frame) != nil {
		return nil
	}
	if frame.StatusCodeValue != nil && *frame.StatusCodeValue != 200 {
		return []qoderEvent{{Err: classifyUpstream(*frame.StatusCodeValue, []byte(frame.Body))}}
	}
	inner := strings.TrimSpace(frame.Body)
	if inner == "" {
		return nil
	}
	if inner == "[DONE]" {
		return nil
	}
	var chunk streamChunk
	if json.Unmarshal([]byte(inner), &chunk) != nil {
		return nil
	}
	if len(chunk.Choices) == 0 && (chunk.Error != nil || (len(chunk.Code) > 0 && chunk.Message != "")) {
		return []qoderEvent{{Err: classifyUpstream(502, []byte(inner))}}
	}
	var out []qoderEvent
	delta := streamDelta{}
	if len(chunk.Choices) > 0 {
		delta = chunk.Choices[0].Delta
		if chunk.Choices[0].FinishReason != nil {
			q.finish = *chunk.Choices[0].FinishReason
		}
	}
	if delta.ReasoningContent != "" {
		out = append(out, qoderEvent{Reasoning: delta.ReasoningContent})
	}
	if delta.Content != nil && *delta.Content != "" {
		out = append(out, qoderEvent{Text: *delta.Content})
	}
	for _, tc := range delta.ToolCalls {
		index := 0
		if tc.Index != nil {
			index = *tc.Index
		}
		name := strings.TrimSpace(tc.Function.Name)
		args := rawArguments(tc.Function.Arguments)
		call := q.native[index]
		if call == nil {
			call = &nativeCall{}
			q.native[index] = call
		}
		if tc.ID != "" {
			call.ID = tc.ID
		}
		if !call.Named {
			call.Args += args
			if name == "" {
				continue
			}
			call.Name = name
			call.Named = true
			q.index++
			call.Index = q.index
			q.sawTool = true
			out = append(out, qoderEvent{Tool: &qoderToolCall{Index: call.Index, ID: call.ID, Name: call.Name, Args: call.Args}})
			continue
		}
		if args != "" {
			call.Args += args
			out = append(out, qoderEvent{Args: &qoderToolArgs{Index: call.Index, Text: args}})
		}
	}
	if u := chunk.Usage; u != nil {
		usage := &openAIUsage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
		if usage.TotalTokens == 0 {
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
		if u.PromptTokensDetails != nil {
			usage.CachedTokens = u.PromptTokensDetails.CachedTokens
		}
		if u.CompletionTokensDetails != nil {
			usage.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
		}
		q.usage = usage
	}
	return out
}

func rawArguments(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// randomChatID is the id an OpenAI answer carries.
func randomChatID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "chatcmpl-000000000000000000000000"
	}
	return "chatcmpl-" + hex.EncodeToString(b)
}
