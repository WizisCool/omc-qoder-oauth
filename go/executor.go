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

// withHTTP is this plan carrying a different signed request, for a retry.
func (p requestPlan) withHTTP(req pluginapi.HTTPRequest) requestPlan {
	p.HTTP = req
	return p
}

// reSignDevice re-signs the same call with the account's device token, for the
// retry a job token the service refuses needs. The encoded body is unchanged:
// only the signature and the token inside it differ.
func (p requestPlan) reSignDevice() (pluginapi.HTTPRequest, bool) {
	c, ok := p.Credential.withDeviceToken()
	if !ok {
		return pluginapi.HTTPRequest{}, false
	}
	spec, ok := siteFor(c.Site)
	if !ok {
		return pluginapi.HTTPRequest{}, false
	}
	headers, err := chatHeaders(p.HTTP.URL, cosyUserFrom(c), string(p.HTTP.Body), spec, getConfig().CosyVersion, p.Model, time.Now().Unix())
	if err != nil {
		return pluginapi.HTTPRequest{}, false
	}
	req := p.HTTP
	req.Headers = headers
	return req, true
}

// chatCall sends one signed call and reports either the stream it answered with,
// the refusal it answered with, or the transport failure that stopped it.
func chatCall(sender httpSender, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, *qoderFailure, error) {
	resp, err := sender.Do(req)
	if err != nil {
		return pluginapi.HTTPResponse{}, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, classifyUpstream(resp.StatusCode, resp.Headers, resp.Body), nil
	}
	if !bytes.Contains(resp.Body, []byte("data:")) {
		// A 200 that is not an SSE stream is Qoder refusing in JSON.
		return resp, classifyUpstream(http.StatusBadGateway, resp.Headers, resp.Body), nil
	}
	return resp, nil, nil
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
	resp, failure, err := chatCall(sender, plan.HTTP)
	if err != nil {
		return pluginapi.ExecutorResponse{}, pluginabi.NewError("transport_error", "Qoder upstream transport failed", 503)
	}
	if failure != nil && failure.Lapsed {
		// A job token the service calls expired may still have a live device
		// token beside it, so the same call is worth one retry signed with it.
		if retry, ok := plan.reSignDevice(); ok {
			if retried, retryFailure, retryErr := chatCall(sender, retry); retryErr == nil {
				switch {
				case retryFailure == nil:
					resp, failure = retried, nil
				case !retryFailure.Lapsed:
					// The device token was accepted but the call still failed:
					// report why the retry was refused rather than re-signing in.
					failure = retryFailure
				}
			}
		}
	}
	if failure != nil {
		return pluginapi.ExecutorResponse{}, decisionError(failure)
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

// closeStreamWithError ends a stream with the structured error the host's router
// reads: without error_detail the host sees a statusless plugin failure and
// answers a mid-stream refusal as a 500, so a quota 429 never reaches the
// routing and cooldown decisions.
func closeStreamWithError(id, message string, err error) {
	var detail *pluginabi.Error
	_ = errors.As(err, &detail)
	if detail == nil {
		closeStream(id, message)
		return
	}
	_ = hostRPC("host.stream.close", map[string]any{"stream_id": id, "error": message, "error_detail": detail}, nil)
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
	readStreamChunk     = readHostStream
	emitStreamFrame     = emitStreamChunk
	openHTTPStream      = openUpstreamStream
	closeHostStreamWith = closeStreamWithError
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
			closeHostStreamWith(p.target, e.Err.Message, decisionError(e.Err))
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
	// A job token the service calls expired may still have a live device token
	// beside it, so a lapsed refusal gets one retry signed with that token.
	plans := []requestPlan{plan}
	if retry, ok := plan.reSignDevice(); ok {
		plans = append(plans, plan.withHTTP(retry))
	}
	for i, p := range plans {
		last := i == len(plans)-1
		response, err := openHTTPStream(p, callbackID)
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
			if readErr != nil {
				release()
				return nil, pluginabi.NewError("transport_error", "Qoder upstream stream failed", 503)
			}
			failure := classifyUpstream(response.StatusCode, response.Headers, raw)
			if failure.Lapsed && !last {
				continue
			}
			release()
			return nil, decisionError(failure)
		}
		events := newQoderEvents(p.Tools)
		writer := newStreamAnswer(modelName(p.Model))
		// Read until the answer starts so a refusal carried inside an OK frame
		// (a queue notice, a lapsed token) keeps its real status instead of
		// arriving as a 200 stream that ends in an error.
		var pending []qoderEvent
		var refused *qoderFailure
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
				refused = e.Err
				closeHostStream(response.StreamID)
				break
			}
		}
		if refused != nil {
			if refused.Lapsed && !last {
				continue
			}
			release()
			return nil, decisionError(refused)
		}
		pump := &streamPump{source: response.StreamID, target: streamID, events: events, writer: writer, task: p.Task}
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
	release()
	return nil, pluginabi.NewError("transport_error", "Qoder upstream stream unavailable", 503)
}

// countTokens is not supported: Qoder documents no token-count endpoint, and a
// guess would misbill clients that trust it.
func countTokens() (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{}, pluginabi.NewError("not_supported", "Qoder does not provide a verified token-count endpoint", http.StatusNotImplemented)
}
