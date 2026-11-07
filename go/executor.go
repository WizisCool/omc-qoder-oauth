package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// chatPath is the SSE endpoint Qoder's client talks to.
const chatPath = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

type credentialGate struct {
	cond   *sync.Cond
	active int
}

var gateMu sync.Mutex
var gates = map[string]*credentialGate{}

// acquire bounds simultaneous requests per credential, as Qoder's plans expect.
func acquire(id string) func() {
	gateMu.Lock()
	g := gates[id]
	if g == nil {
		g = &credentialGate{cond: sync.NewCond(&gateMu)}
		gates[id] = g
	}
	for g.active >= getConfig().MaxConcurrency {
		g.cond.Wait()
	}
	g.active++
	gateMu.Unlock()
	return func() { gateMu.Lock(); g.active--; g.cond.Broadcast(); gateMu.Unlock() }
}

type requestPlan struct {
	HTTP       pluginapi.HTTPRequest
	Chat       chatRequest
	Model      qoderModel
	Session    string
	Task       *qoderTask
	Tools      []chatTool
	Credential credential
}

// prepareRequest turns an OpenAI chat completion into a signed Qoder request.
func prepareRequest(req pluginapi.ExecutorRequest, sender httpSender) (requestPlan, error) {
	var plan requestPlan
	c, err := readCredential(req.StorageJSON)
	if err != nil {
		return plan, pluginabi.NewError("invalid_auth", "invalid Qoder credential", 401)
	}
	spec, ok := siteFor(c.Site)
	if !ok {
		return plan, pluginabi.NewError("invalid_auth", "invalid Qoder credential", 401)
	}
	model := trimModelPrefix(strings.TrimSpace(req.Model))
	if model == "" {
		return plan, pluginabi.NewError("invalid_request", "model is required", 400)
	}
	chat, err := parseChatRequest(req.Payload)
	if err != nil {
		return plan, pluginabi.NewError("invalid_request", "invalid chat completion request", 400)
	}
	m, ok := findModel(sender, c, spec, model)
	if !ok {
		return plan, pluginabi.NewError("unknown_model", fmt.Sprintf("unknown or disabled Qoder model %q", model), 400)
	}
	session := sessionID(chat, req.Headers.Get("x-session-id"))
	task := qoderTasks.begin(session, chat, time.Now().UnixMilli())
	body, err := buildQoderBody(chat, m, session, task, getConfig().CosyVersion, getConfig().MaxTokens)
	if err != nil {
		return plan, pluginabi.NewError("invalid_request", err.Error(), 400)
	}
	target := spec.API + chatPath
	wire := encodeBody(body)
	headers, err := chatHeaders(target, cosyUserFrom(c), wire, spec, getConfig().CosyVersion, m, time.Now().Unix())
	if err != nil {
		return plan, pluginabi.NewError("invalid_auth", err.Error(), 401)
	}
	return requestPlan{
		HTTP:       pluginapi.HTTPRequest{Method: "POST", URL: target, Headers: headers, Body: []byte(wire)},
		Chat:       chat,
		Model:      m,
		Session:    session,
		Task:       task,
		Tools:      chat.Tools,
		Credential: c,
	}, nil
}

// execute answers one non-streaming chat completion. Qoder always streams, so
// the buffered body is read as SSE either way.
func execute(req pluginapi.ExecutorRequest, sender httpSender) (pluginapi.ExecutorResponse, error) {
	plan, err := prepareRequest(req, sender)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	release := acquire(req.AuthID)
	defer release()
	resp, err := sender.Do(plan.HTTP)
	if err != nil {
		return pluginapi.ExecutorResponse{}, pluginabi.NewError("transport_error", "Qoder upstream transport failed", 503)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.ExecutorResponse{}, decisionError(classifyUpstream(resp.StatusCode, resp.Body))
	}
	if !bytes.Contains(resp.Body, []byte("data:")) {
		// A 200 that is not an SSE stream is Qoder refusing in JSON.
		return pluginapi.ExecutorResponse{}, decisionError(classifyUpstream(http.StatusBadGateway, resp.Body))
	}
	events := newQoderEvents(plan.Tools)
	var out []qoderEvent
	out = append(out, events.feed(resp.Body)...)
	out = append(out, events.flush()...)
	for _, e := range out {
		if e.Err != nil {
			return pluginapi.ExecutorResponse{}, decisionError(e.Err)
		}
	}
	payload, failure := completionFromEvents(modelName(plan.Model), out)
	if failure != nil {
		return pluginapi.ExecutorResponse{}, decisionError(failure)
	}
	plan.Task.markAnswered(time.Now().UnixMilli())
	return pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": {"application/json"}},
		Metadata: map[string]any{
			"upstream_host":  "qoder",
			"upstream_model": plan.Model.Key,
		},
	}, nil
}

// modelName is the id an answer reports. It is the published readable id, so a
// panel or a usage record names "glm-5.3" rather than Qoder's internal key.
func modelName(m qoderModel) string {
	if id := strings.ToLower(strings.TrimSpace(m.ID)); id != "" {
		return id
	}
	if key := strings.ToLower(strings.TrimSpace(m.Key)); key != "" {
		return key
	}
	return m.Name
}

type hostStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}

type hostReadResponse struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

func readHostStream(id string) (hostReadResponse, error) {
	var r hostReadResponse
	if err := hostRPC("host.http.stream_read", map[string]any{"stream_id": id}, &r); err != nil {
		return r, errors.New("upstream stream read failed")
	}
	if r.Error != "" {
		return r, errors.New("upstream stream failed")
	}
	return r, nil
}

func closeHostStream(id string) {
	_ = hostRPC("host.http.stream_close", map[string]any{"stream_id": id}, nil)
}

func emitStreamChunk(id string, payload []byte) error {
	return hostRPC("host.stream.emit", map[string]any{"stream_id": id, "payload": payload}, nil)
}

func closeStream(id, message string) {
	_ = hostRPC("host.stream.close", map[string]any{"stream_id": id, "error": message}, nil)
}

// readWholeStream drains and closes a host HTTP stream, with a bound so a
// misbehaving upstream cannot exhaust memory.
func readWholeStream(id string) ([]byte, error) {
	defer closeHostStream(id)
	var out []byte
	for {
		r, err := readHostStream(id)
		if err != nil {
			return nil, err
		}
		out = append(out, r.Payload...)
		if len(out) > 1<<20 {
			return nil, errors.New("upstream error body too large")
		}
		if r.Done {
			return out, nil
		}
	}
}

// Streaming seams. Tests replace them to exercise the pump without a plugin host.
var (
	readStreamChunk = readHostStream
	emitStreamFrame = emitStreamChunk
	openHTTPStream  = openUpstreamStream
)

func openUpstreamStream(plan requestPlan, callbackID string) (hostStreamResponse, error) {
	var response hostStreamResponse
	err := hostRPC("host.http.do_stream", map[string]any{"host_callback_id": callbackID, "request": plan.HTTP}, &response)
	return response, err
}

// streamPump copies one Qoder SSE stream into the host stream as OpenAI chunks.
type streamPump struct {
	source  string
	target  string
	events  *qoderEvents
	writer  *streamAnswer
	task    *qoderTask
	emitted bool
}

func (p *streamPump) deliver(events []qoderEvent) bool {
	for _, e := range events {
		if e.Err != nil {
			closeStream(p.target, e.Err.Message)
			return false
		}
		for _, frame := range p.writer.frames(e) {
			if emitStreamFrame(p.target, frame) != nil {
				closeStream(p.target, "client stream closed")
				return false
			}
			p.emitted = true
		}
	}
	return true
}

func (p *streamPump) run() {
	for {
		r, err := readStreamChunk(p.source)
		if err != nil {
			closeStream(p.target, "Qoder stream failed")
			return
		}
		if !p.deliver(p.events.feed(r.Payload)) {
			return
		}
		if r.Done {
			if !p.deliver(p.events.flush()) {
				return
			}
			if !p.writer.saidSomething() {
				closeStream(p.target, "Qoder ended without an answer")
				return
			}
			if frame := p.writer.done(); len(frame) > 0 {
				_ = emitStreamFrame(p.target, frame)
			}
			p.task.markAnswered(time.Now().UnixMilli())
			closeStream(p.target, "")
			return
		}
	}
}

// executeStream opens the upstream stream, rejects a refusal before any chunk
// reaches the client, and then pumps asynchronously.
func executeStream(req pluginapi.ExecutorRequest, callbackID, streamID string, sender httpSender) (map[string]any, error) {
	if streamID == "" {
		return nil, pluginabi.NewError("invalid_request", "stream ID required", 400)
	}
	plan, err := prepareRequest(req, sender)
	if err != nil {
		return nil, err
	}
	release := acquire(req.AuthID)
	response, err := openHTTPStream(plan, callbackID)
	if err != nil {
		release()
		return nil, pluginabi.NewError("transport_error", "Qoder upstream stream failed", 503)
	}
	if response.StreamID == "" {
		release()
		return nil, pluginabi.NewError("transport_error", "Qoder upstream stream unavailable", 503)
	}
	contentType := strings.ToLower(response.Headers.Get("Content-Type"))
	if response.StatusCode < 200 || response.StatusCode >= 300 || strings.Contains(contentType, "application/json") {
		raw, readErr := readWholeStream(response.StreamID)
		release()
		if readErr != nil {
			return nil, pluginabi.NewError("transport_error", "Qoder upstream stream failed", 503)
		}
		return nil, decisionError(classifyUpstream(response.StatusCode, raw))
	}
	events := newQoderEvents(plan.Tools)
	writer := newStreamAnswer(modelName(plan.Model))
	// Read until the answer starts so a refusal carried inside an OK frame
	// (a queue notice, a lapsed token) keeps its real status instead of
	// arriving as a 200 stream that ends in an error.
	var pending []qoderEvent
	for attempt := 0; attempt < 8 && len(pending) == 0; attempt++ {
		r, err := readStreamChunk(response.StreamID)
		if err != nil {
			closeHostStream(response.StreamID)
			release()
			return nil, pluginabi.NewError("transport_error", "Qoder upstream stream failed", 503)
		}
		pending = append(pending, events.feed(r.Payload)...)
		if r.Done {
			pending = append(pending, events.flush()...)
			break
		}
	}
	for _, e := range pending {
		if e.Err != nil {
			closeHostStream(response.StreamID)
			release()
			return nil, decisionError(e.Err)
		}
	}
	pump := &streamPump{source: response.StreamID, target: streamID, events: events, writer: writer, task: plan.Task}
	go func() {
		defer release()
		defer closeHostStream(pump.source)
		if !pump.deliver(pending) {
			return
		}
		pump.run()
	}()
	return map[string]any{"headers": response.Headers}, nil
}

// countTokens is not supported: Qoder documents no token-count endpoint, and a
// guess would misbill clients that trust it.
func countTokens() (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{}, pluginabi.NewError("not_supported", "Qoder does not provide a verified token-count endpoint", http.StatusNotImplemented)
}
