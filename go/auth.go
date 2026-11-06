package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// signInTimeout is how long the browser flow may stay open. Qoder's own
	// client allows 15 minutes.
	signInTimeout = 15 * time.Minute
	// refreshLead is how early the host should renew a job token.
	refreshLead = 10 * time.Minute
	// day is the fallback lifetime of a token that names no end.
	day = 24 * time.Hour
)

var phoneEmailPattern = regexp.MustCompile(`^phone_[0-9a-f-]+@phone\.local$`)

// expiredError marks a sign-in the vendor refused: the host must ask the user
// to sign in again rather than retry the refresh.
type expiredError struct{ message string }

func (e *expiredError) Error() string { return e.message }

func signInExpired(spec siteSpec) error {
	return &expiredError{message: spec.Name + " sign-in has expired; sign in again"}
}

// startLogin begins a PKCE device flow on the selected portal. The verifier is
// derived from the sealed state's nonce, so the opaque state CPA carries stays
// inside its 128-character limit.
func startLogin(req pluginapi.AuthLoginStartRequest, sender httpSender) (pluginapi.AuthLoginStartResponse, error) {
	var out pluginapi.AuthLoginStartResponse
	if req.Host.AuthDir == "" {
		return out, errors.New("CPA auth directory unavailable")
	}
	spec, err := loginSite(req.Provider, req.Metadata, getConfig().Site)
	if err != nil {
		return out, err
	}
	key, err := loadStateKey(req.Host.AuthDir)
	if err != nil {
		return out, err
	}
	nonce, err := newNonce()
	if err != nil {
		return out, errors.New("cannot generate login nonce")
	}
	machineID, err := newNonce()
	if err != nil {
		return out, errors.New("cannot generate login machine id")
	}
	verifier := deriveVerifier(key, nonce)
	query := url.Values{
		"challenge":        {deriveChallenge(verifier)},
		"challenge_method": {"S256"},
		"nonce":            {uuidString(nonce)},
		"machine_id":       {uuidString(machineID)},
		"client_id":        {spec.ClientID},
	}
	// No redirect_uri, as Qoder's CLI sends none: the vendor page finishes in
	// the browser tab and the plugin polls the same flow.
	expires := time.Now().Add(signInTimeout)
	state, err := sealState(key, loginState{Site: spec.ID, Expires: expires.Unix(), Nonce: nonce, MachineID: machineID})
	if err != nil {
		return out, err
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  credentialProvider,
		URL:       spec.Web + "/device/selectAccounts?" + query.Encode(),
		State:     state,
		ExpiresAt: expires.UTC(),
	}, nil
}

// loginSite resolves the portal: the entry the request names wins, then an
// explicit metadata hint, then the configured fallback, then qoder.com. An
// invalid explicit value is an error rather than a silent fallback.
func loginSite(provider string, metadata map[string]any, configured string) (siteSpec, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "":
		// A legacy panel that knows only the plugin key sends no provider.
	case pluginStoreID:
		// Same fallback path: the store identifier is not a login entry.
	case siteQoder, siteQoderCN:
		if spec, ok := siteByEntry(strings.ToLower(strings.TrimSpace(provider))); ok {
			return spec, nil
		}
	default:
		return siteSpec{}, errors.New("unknown login entry: use qoder or qoder-cn")
	}
	site := configured
	if raw, present := metadata["site"]; present {
		value, ok := raw.(string)
		if !ok {
			return siteSpec{}, errors.New("invalid login site: use qoder or qoder-cn")
		}
		site = strings.ToLower(strings.TrimSpace(value))
	}
	if site == "" {
		site = siteQoder
	}
	spec, ok := siteFor(site)
	if !ok {
		return siteSpec{}, errors.New("invalid login site: use qoder or qoder-cn")
	}
	return spec, nil
}

type deviceToken struct {
	Token        string
	Refresh      string
	Expires      int64
	UserID       string
	UserName     string
	ExpiresAtRaw json.RawMessage
	ExpiresIn    float64
}

// pollLogin asks the vendor whether the browser flow finished, and on success
// exchanges the device token for the job token model calls use.
func pollLogin(req pluginapi.AuthLoginPollRequest, sender httpSender) (pluginapi.AuthLoginPollResponse, error) {
	pending := pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending}
	key, err := loadStateKey(req.Host.AuthDir)
	if err != nil {
		return pending, err
	}
	s, err := openState(key, req.State)
	if err != nil {
		return pending, err
	}
	if time.Now().Unix() >= s.Expires {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "Qoder sign-in expired; sign in again"}, nil
	}
	spec, _ := siteFor(s.Site)
	token, finished, err := pollDeviceToken(sender, spec, deriveVerifier(key, s.Nonce), uuidString(s.Nonce))
	if err != nil {
		return pending, err
	}
	if finished {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "Qoder sign-in was declined"}, nil
	}
	if token.Token == "" {
		return pending, nil
	}
	c, err := finishLogin(sender, spec, s.MachineID, token)
	if err != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: err.Error()}, nil
	}
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: authFor(c)}, nil
}

// pollDeviceToken asks the device token endpoint once. A 200 with a token is
// success; a refusal that means the user declined ends the flow; anything else
// (no authorization yet, a transient upstream failure) stays pending.
func pollDeviceToken(sender httpSender, spec siteSpec, verifier, nonce string) (deviceToken, bool, error) {
	var out deviceToken
	query := url.Values{"nonce": {nonce}, "verifier": {verifier}, "challenge_method": {"S256"}}
	resp, err := sender.Do(pluginapi.HTTPRequest{
		Method:  "GET",
		URL:     spec.OpenAPI + "/api/v1/deviceToken/poll?" + query.Encode(),
		Headers: map[string][]string{"Accept": {"application/json"}, "User-Agent": {"Qoder"}},
	})
	if err != nil {
		return out, false, nil
	}
	if resp.StatusCode == 403 {
		return out, true, nil
	}
	if resp.StatusCode != 200 {
		return out, false, nil
	}
	var body struct {
		Token     string          `json:"token"`
		Refresh   string          `json:"refresh_token"`
		ExpiresAt json.RawMessage `json:"expires_at"`
		ExpiresIn float64         `json:"expires_in"`
		UserID    string          `json:"user_id"`
		UserName  string          `json:"user_name"`
	}
	if json.Unmarshal(resp.Body, &body) != nil || strings.TrimSpace(body.Token) == "" {
		return out, false, nil
	}
	out = deviceToken{Token: strings.TrimSpace(body.Token), Refresh: strings.TrimSpace(body.Refresh), UserID: strings.TrimSpace(body.UserID), UserName: strings.TrimSpace(body.UserName), ExpiresAtRaw: body.ExpiresAt, ExpiresIn: body.ExpiresIn}
	out.Expires = deviceExpiry(out)
	return out, false, nil
}

// finishLogin trades the device token for a job token, reads the account and
// returns the credential to persist.
func finishLogin(sender httpSender, spec siteSpec, machineID []byte, token deviceToken) (credential, error) {
	var c credential
	access, refresh, expires := "", "", int64(0)
	deviceChat := false
	job, err := openAPI(sender, "POST", spec.OpenAPI+"/api/v1/me/jobToken", token.Token, map[string]string{"clientId": spec.ClientID})
	if err == nil {
		var jt struct {
			Token     string          `json:"token"`
			Refresh   string          `json:"refresh_token"`
			Expire    json.RawMessage `json:"expire_time"`
			ExpiresAt json.RawMessage `json:"expires_at"`
			ExpiresIn float64         `json:"expires_in"`
		}
		if json.Unmarshal(job, &jt) != nil || strings.TrimSpace(jt.Token) == "" {
			return c, errors.New(spec.Name + " job token: empty token in response")
		}
		access = strings.TrimSpace(jt.Token)
		refresh = strings.TrimSpace(jt.Refresh)
		expires = expiresAt(jt.Expire, jt.ExpiresAt)
		if expires == 0 {
			expires = expiresAtIn(jt.ExpiresIn)
		}
		if expires == 0 {
			expires = time.Now().Add(day).UnixMilli()
		}
	} else if spec.ID == siteQoderCN && statusOf(err) >= 400 && statusOf(err) < 500 {
		// Qoder CN's CLI never makes a job token: its device token is what
		// signs the model calls.
		access, refresh, expires, deviceChat = "", token.Refresh, token.Expires, true
	} else {
		return c, fmt.Errorf("%s job token: %s", spec.Name, readableFailure(err))
	}
	email, name, uid, phone := userInfo(sender, spec, token.Token)
	if uid == "" {
		uid = strings.TrimSpace(token.UserID)
	}
	if uid == "" {
		uid = realEmail(email)
	}
	if uid == "" {
		uid = strings.TrimSpace(name)
	}
	if uid == "" {
		return c, errors.New(spec.Name + ": the sign-in named no user")
	}
	c = credential{
		Site:          spec.ID,
		Access:        access,
		Refresh:       refresh,
		Expires:       expires,
		DeviceToken:   token.Token,
		DeviceRefresh: token.Refresh,
		DeviceChat:    deviceChat,
		UID:           uid,
		Email:         email,
		Name:          name,
		Phone:         phone,
		MachineID:     uuidString(machineID),
	}
	return c, nil
}

// userInfo reads the account identity. Failure is not fatal: the device token
// response usually names the user, and the account still works without a name.
func userInfo(sender httpSender, spec siteSpec, token string) (email, name, uid, phone string) {
	raw, err := openAPI(sender, "GET", spec.OpenAPI+"/api/v1/userinfo", token, nil)
	if err != nil {
		return "", "", "", ""
	}
	var u struct {
		Email  string `json:"email"`
		Name   string `json:"name"`
		ID     string `json:"id"`
		UserID string `json:"user_id"`
		UID    string `json:"uid"`
		Mobile string `json:"security_mobile"`
		Phone  string `json:"phone"`
		Number string `json:"phoneNumber"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return "", "", "", ""
	}
	uid = firstNonEmpty(u.ID, u.UserID, u.UID)
	phone = firstNonEmpty(u.Mobile, u.Phone, u.Number)
	return strings.TrimSpace(u.Email), strings.TrimSpace(u.Name), strings.TrimSpace(uid), strings.TrimSpace(phone)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// labelFor is the name a card shows: a real email, then a masked phone, then
// the account name, then the uid.
func labelFor(email, name, phone, uid string) string {
	if e := realEmail(email); e != "" {
		return e
	}
	if p := maskPhone(phone); p != "" {
		return p
	}
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	if e := strings.TrimSpace(email); e != "" {
		return e
	}
	return strings.TrimSpace(uid)
}

// maskPhone hides the middle of a Chinese mobile number, as Qoder's own panels do.
func maskPhone(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	digits := b.String()
	if strings.HasPrefix(digits, "0086") && len(digits)-4 >= 7 {
		digits = digits[4:]
	} else if strings.HasPrefix(digits, "86") && len(digits)-2 >= 7 {
		digits = digits[2:]
	}
	if len(digits) < 11 {
		return ""
	}
	return digits[:3] + "****" + digits[len(digits)-4:]
}

// expiresAt reads a token end the way Qoder's client does: expire_time or
// expires_at (a date, or an epoch in seconds or milliseconds), then expires_in
// (milliseconds), then a day.
func expiresAt(values ...json.RawMessage) int64 {
	for _, raw := range values {
		if at := whenSaid(raw); at > 0 {
			return at
		}
	}
	return 0
}

func expiresAtIn(expiresIn any) int64 {
	switch v := expiresIn.(type) {
	case float64:
		if v > 0 {
			return time.Now().UnixMilli() + int64(v)
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			return time.Now().UnixMilli() + n
		}
	}
	return 0
}

// whenSaid is the time a token's end field names, in milliseconds: a date, or
// an epoch in seconds or milliseconds; 0 for none.
func whenSaid(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0
		}
		if isDigits(s) {
			return epochMilli(s)
		}
		if at, err := time.Parse(time.RFC3339, s); err == nil {
			return at.UnixMilli()
		}
		for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
			if at, err := time.Parse(layout, s); err == nil {
				return at.UnixMilli()
			}
		}
		return 0
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return epochMilli(n.String())
	}
	return 0
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func epochMilli(s string) int64 {
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		if n > (1<<62)/10 {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	if n <= 0 {
		return 0
	}
	if n < 1e12 {
		return n * 1000
	}
	return n
}

// deviceExpiry is when a device token used as the chat token runs out: its
// expires_at (a date) or expires_in (seconds), a day when it names neither.
func deviceExpiry(t deviceToken) int64 {
	if at := whenSaid(t.ExpiresAtRaw); at > time.Now().UnixMilli() {
		return at
	}
	if t.ExpiresIn > 0 {
		return time.Now().UnixMilli() + int64(t.ExpiresIn*1000)
	}
	return time.Now().Add(day).UnixMilli()
}

// authFor maps a credential to the auth record CPA persists.
func authFor(c credential) pluginapi.AuthData {
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256([]byte(c.Site + "\x00" + c.UID))
	id := "qoder-" + hex.EncodeToString(sum[:12]) + ".json"
	label := c.label()
	return pluginapi.AuthData{
		Provider:         credentialProvider,
		ID:               id,
		FileName:         id,
		Label:            label,
		StorageJSON:      raw,
		Metadata:         map[string]any{"email": label, "type": credentialProvider, "oauth_provider": c.Site},
		NextRefreshAfter: nextRefresh(c),
	}
}

func nextRefresh(c credential) time.Time {
	now := time.Now()
	at := c.expiry()
	if at.IsZero() {
		return now.Add(refreshLead)
	}
	next := at.Add(-refreshLead)
	if next.Before(now.Add(time.Minute)) {
		return now.Add(time.Minute)
	}
	return next
}

// refreshCredential spends the refresh token once. Qoder's refresh tokens are
// single-use, so a concurrent call for the same old pair waits for the first
// result instead of spending it twice.
func refreshCredential(req pluginapi.AuthRefreshRequest, sender httpSender) (pluginapi.AuthRefreshResponse, error) {
	var out pluginapi.AuthRefreshResponse
	c, err := readCredential(req.StorageJSON)
	if err != nil {
		return out, errors.New("invalid qoder credential")
	}
	spec, _ := siteFor(c.Site)
	if strings.TrimSpace(c.Refresh) == "" {
		return out, signInExpired(spec)
	}
	refreshed, err := renewJobToken(req.AuthID, sender, spec, c)
	if err != nil {
		return out, err
	}
	raw, _ := json.Marshal(refreshed)
	out = pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:         credentialProvider,
			ID:               req.AuthID,
			Label:            refreshed.label(),
			StorageJSON:      raw,
			Metadata:         map[string]any{"email": refreshed.label(), "type": credentialProvider, "oauth_provider": refreshed.Site},
			NextRefreshAfter: nextRefresh(refreshed),
		},
		NextRefreshAfter: nextRefresh(refreshed),
	}
	return out, nil
}

type refreshFlight struct {
	from   string
	done   chan struct{}
	result credential
	err    error
}

var refreshMu sync.Mutex
var refreshFlights = map[string]*refreshFlight{}

func renewJobToken(authID string, sender httpSender, spec siteSpec, c credential) (credential, error) {
	key := authID
	if key == "" {
		key = c.Site + "\x00" + c.UID
	}
	refreshMu.Lock()
	if flight := refreshFlights[key]; flight != nil {
		refreshMu.Unlock()
		<-flight.done
		if flight.from == c.Refresh {
			if flight.err != nil {
				return credential{}, flight.err
			}
			return flight.result, nil
		}
		// The saved pair changed while we waited; ask with the caller's own.
		return renewOnce(sender, spec, c)
	}
	flight := &refreshFlight{from: c.Refresh, done: make(chan struct{})}
	refreshFlights[key] = flight
	refreshMu.Unlock()

	flight.result, flight.err = renewOnce(sender, spec, c)
	refreshMu.Lock()
	delete(refreshFlights, key)
	refreshMu.Unlock()
	close(flight.done)
	if flight.err != nil {
		return credential{}, flight.err
	}
	return flight.result, nil
}

func renewOnce(sender httpSender, spec siteSpec, c credential) (credential, error) {
	raw, err := openAPI(sender, "POST", spec.OpenAPI+"/api/v1/jobToken/refresh", "", map[string]string{"refresh_token": c.Refresh})
	if err != nil {
		if status := statusOf(err); status == 401 || status == 403 {
			return credential{}, signInExpired(spec)
		}
		return credential{}, fmt.Errorf("%s job token refresh failed: %s", spec.Name, readableFailure(err))
	}
	var jt struct {
		Token     string          `json:"token"`
		Refresh   string          `json:"refresh_token"`
		Expire    json.RawMessage `json:"expire_time"`
		ExpiresAt json.RawMessage `json:"expires_at"`
		ExpiresIn float64         `json:"expires_in"`
	}
	if json.Unmarshal(raw, &jt) != nil || strings.TrimSpace(jt.Token) == "" || strings.TrimSpace(jt.Refresh) == "" {
		return credential{}, errors.New(spec.Name + " job token refresh returned an incomplete pair")
	}
	next := c
	next.Access = strings.TrimSpace(jt.Token)
	next.Refresh = strings.TrimSpace(jt.Refresh)
	next.DeviceChat = false
	if at := expiresAt(jt.Expire, jt.ExpiresAt); at > 0 {
		next.Expires = at
	} else if at := expiresAtIn(jt.ExpiresIn); at > 0 {
		next.Expires = at
	} else {
		next.Expires = time.Now().Add(day).UnixMilli()
	}
	return next, nil
}

func readableFailure(err error) string {
	if err == nil {
		return "unknown error"
	}
	var e *apiError
	if errors.As(err, &e) {
		msg := strings.TrimSpace(e.Message)
		if msg == "" {
			msg = http.StatusText(e.Status)
		}
		if e.Code != "" {
			return fmt.Sprintf("HTTP %d, code %s: %s", e.Status, e.Code, msg)
		}
		return fmt.Sprintf("HTTP %d: %s", e.Status, msg)
	}
	return err.Error()
}

// parseAuth recognizes an existing "type": "qoder" auth file and republishes it.
func parseAuth(req pluginapi.AuthParseRequest) pluginapi.AuthParseResponse {
	switch strings.ToLower(strings.TrimSpace(req.Provider)) {
	case "", credentialProvider, siteQoderCN, pluginStoreID:
	default:
		return pluginapi.AuthParseResponse{}
	}
	var metadata struct {
		Type  string `json:"type"`
		Email string `json:"email"`
	}
	if json.Unmarshal(req.RawJSON, &metadata) != nil || metadata.Type != credentialProvider {
		return pluginapi.AuthParseResponse{}
	}
	c, err := readCredential(req.RawJSON)
	if err != nil {
		return pluginapi.AuthParseResponse{}
	}
	name := req.FileName
	if name == "" {
		name = req.Path
	}
	label := strings.TrimSpace(metadata.Email)
	if label == "" {
		label = c.label()
	}
	a := pluginapi.AuthData{
		Provider:    credentialProvider,
		ID:          name,
		FileName:    req.FileName,
		Label:       label,
		StorageJSON: append([]byte(nil), req.RawJSON...),
		Metadata:    map[string]any{"email": label, "type": credentialProvider, "oauth_provider": c.Site},
	}
	a.NextRefreshAfter = nextRefresh(c)
	return pluginapi.AuthParseResponse{Handled: true, Auth: a}
}
