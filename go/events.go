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
	dec     sseDecoder
	split   *splitter
	native  map[int]*nativeCall
	index   int
	finish  string
	usage   *openAIUsage
	sawTool bool
	// said is whether any of the answer went out: text, reasoning or a call,
	// which a client may already show.
	said bool
	// done is Qoder's [DONE]: nothing more is read, but the answer still ends.
	done bool
	// closed is an answer that already ended, in an error or its stop.
	closed bool
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
	return &qoderEvents{split: newSplitter(tools), native: map[int]*nativeCall{}, index: -1}
}

func (q *qoderEvents) feed(chunk []byte) []qoderEvent {
	var out []qoderEvent
	for _, line := range q.dec.feed(chunk) {
		out = append(out, q.line(line)...)
	}
	return out
}

// toolEmitted reports whether the answer carried a tool call, which makes the
// stop reason tool_calls.
func (q *qoderEvents) toolEmitted() bool { return q.sawTool }

// flush ends the answer: the text still held, the calls in it, and the stop with
// the usage Qoder sent after its finish.
func (q *qoderEvents) flush() []qoderEvent {
	var out []qoderEvent
	for _, line := range q.dec.flush() {
		out = append(out, q.line(line)...)
	}
	if q.closed {
		return out
	}
	pieces, ok := q.pieces(q.split.flush())
	out = append(out, pieces...)
	if !ok {
		q.closed = true
		return out
	}
	// A native call the model began and never named is never given: a client
	// keeps an unnamed tool in its history and other providers refuse it. Its
	// arguments, held while it waited for the name, go nowhere, not into the
	// text either. With nothing said before it the reply is no answer: a 502
	// the executor may retry. After an answer began the call is dropped and the
	// answer ends as it would have without it, so the text, reasoning and named
	// calls already given are kept.
	nameless := 0
	for _, call := range q.native {
		if !call.Named {
			nameless++
		}
	}
	if nameless > 0 && !q.said {
		q.closed = true
		return append(out, qoderEvent{Err: &qoderFailure{Status: 502, Message: namelessCallText, Nameless: true}})
	}
	q.closed = true
	stop := "stop"
	if q.sawTool {
		stop = "tool_calls"
	} else if q.finish == "length" {
		stop = "length"
	}
	return append(out, qoderEvent{Stop: stop, Usage: q.usage})
}

// pieces turns the splitter's findings into events: a call takes the next
// client-facing index and an id of its own, text passes through, and a call
// that can't be read ends the turn as a 502.
func (q *qoderEvents) pieces(found []callPiece) ([]qoderEvent, bool) {
	var out []qoderEvent
	for _, f := range found {
		switch {
		case f.Call != nil:
			q.index++
			q.sawTool = true
			q.said = true
			out = append(out, qoderEvent{Tool: &qoderToolCall{Index: q.index, ID: randomCallID(), Name: f.Call.Name, Args: f.Call.Args}})
		case f.Error != "":
			return append(out, qoderEvent{Err: &qoderFailure{Status: 502, Message: f.Error}}), false
		case f.Text != "":
			q.said = true
			out = append(out, qoderEvent{Text: f.Text})
		}
	}
	return out, true
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
	if q.closed || q.done || !strings.HasPrefix(line, "data:") {
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
	inner := strings.TrimSpace(frame.Body)
	if frame.StatusCodeValue != nil && *frame.StatusCodeValue != 200 {
		body := inner
		if body == "" {
			body = payload
		}
		q.closed = true
		return []qoderEvent{{Err: classifyUpstream(*frame.StatusCodeValue, []byte(body))}}
	}
	if inner == "" {
		return nil
	}
	if inner == "[DONE]" {
		q.done = true
		return nil
	}
	var chunk streamChunk
	if json.Unmarshal([]byte(inner), &chunk) != nil {
		return nil
	}
	// A refusal said inside an OK frame (a Lingma 1.0 domain's "Model not
	// exist" or 105) is the turn's failure, not a chunk to read past into an
	// empty answer.
	if len(chunk.Choices) == 0 && (chunk.Error != nil || (len(chunk.Code) > 0 && chunk.Message != "")) {
		q.closed = true
		return []qoderEvent{{Err: classifyUpstream(502, []byte(inner))}}
	}
	var out []qoderEvent
	delta := streamDelta{}
	if len(chunk.Choices) > 0 {
		delta = chunk.Choices[0].Delta
		if finish := chunk.Choices[0].FinishReason; finish != nil && *finish != "" {
			q.finish = *finish
		}
	}
	if delta.ReasoningContent != "" {
		q.said = true
		out = append(out, qoderEvent{Reasoning: delta.ReasoningContent})
	}
	if delta.Content != nil {
		pieces, ok := q.pieces(q.split.feed(*delta.Content))
		out = append(out, pieces...)
		if !ok {
			q.closed = true
			return out
		}
	}
	for _, tc := range delta.ToolCalls {
		index := 0
		if tc.Index != nil {
			index = *tc.Index
		}
		name := strings.TrimSpace(tc.Function.Name)
		args := rawArguments(tc.Function.Arguments)
		if tc.ID == "" && name == "" && args == "" {
			continue
		}
		call := q.native[index]
		if call == nil {
			call = &nativeCall{}
			q.native[index] = call
		}
		if tc.ID != "" {
			call.ID = tc.ID
		}
		if !call.Named {
			// An id or arguments can arrive before the name. Never give an
			// unnamed tool: clients keep it and other providers refuse it.
			call.Args += args
			if name == "" {
				continue
			}
			pieces, ok := q.pieces(q.split.flush())
			out = append(out, pieces...)
			if !ok {
				q.closed = true
				return out
			}
			if call.ID == "" {
				call.ID = randomCallID()
			}
			q.index++
			call.Index, call.Name, call.Named = q.index, name, true
			q.sawTool, q.said = true, true
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
