package main

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// The two Qoder sites are separate accounts on separate hosts. Both produce the
// same credential shape ("type": "qoder") and share one executor; the site field
// records which portal signed the account in.
const (
	siteQoder   = "qoder"
	siteQoderCN = "qoder-cn"

	// credentialProvider is the provider key every credential this plugin writes
	// carries, and the identifier CPA routes executions and quota by.
	credentialProvider = "qoder"

	// defaultCosyVersion is the Qoder desktop build the COSY envelope claims.
	// It is configuration so a vendor build bump does not need a new release.
	defaultCosyVersion = "1.1.69"
)

type siteSpec struct {
	ID       string
	Name     string
	Web      string
	OpenAPI  string
	API      string
	ClientID string
	// LogoAsset is the embedded asset served for this portal's login entry.
	LogoAsset string
	// Fallback lists the models the site ships when its catalogue cannot be read.
	Fallback []qoderModelSeed
}

var sites = []siteSpec{
	{
		ID:        siteQoder,
		Name:      "Qoder",
		Web:       "https://qoder.com",
		OpenAPI:   "https://openapi.qoder.sh",
		API:       "https://api3.qoder.sh",
		ClientID:  "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa",
		LogoAsset: "qoder-color.svg",
		Fallback:  qoderFallbackCatalogue,
	},
	{
		ID:        siteQoderCN,
		Name:      "Qoder CN",
		Web:       "https://qoder.cn",
		OpenAPI:   "https://openapi.qoder.com.cn",
		API:       "https://gateway.qoder.com.cn",
		ClientID:  "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb",
		LogoAsset: "qoder.svg",
		// Qoder CN serves the same catalogue on its own gateway; its shipped
		// list is the tiers its CLI names, and the account's list takes over
		// once the account is signed in.
		Fallback: qoderCNFallbackCatalogue,
	},
}

func siteFor(id string) (siteSpec, bool) {
	for _, s := range sites {
		if s.ID == id {
			return s, true
		}
	}
	return siteSpec{}, false
}

func siteByEntry(entry string) (siteSpec, bool) {
	for _, s := range sites {
		if s.ID == entry {
			return s, true
		}
	}
	return siteSpec{}, false
}

// credential is the persisted auth storage. Tokens are opaque strings; Expires
// is the job token's end in milliseconds since the epoch (0 when Qoder named no
// end). DeviceChat records a Qoder CN account whose device token is the chat
// token because the site refused to trade it for a job token.
type credential struct {
	Site          string `json:"site"`
	Access        string `json:"access,omitempty"`
	Refresh       string `json:"refresh,omitempty"`
	Expires       int64  `json:"expires,omitempty"`
	DeviceToken   string `json:"device_token,omitempty"`
	DeviceRefresh string `json:"device_refresh,omitempty"`
	DeviceChat    bool   `json:"device_chat,omitempty"`
	UID           string `json:"uid"`
	Email         string `json:"email,omitempty"`
	Name          string `json:"name,omitempty"`
	Phone         string `json:"phone,omitempty"`
	MachineID     string `json:"machine_id"`
	Plan          string `json:"plan,omitempty"`
}

func readCredential(b []byte) (credential, error) {
	var c credential
	if json.Unmarshal(b, &c) != nil {
		return c, errors.New("invalid qoder credential")
	}
	spec, ok := siteFor(c.Site)
	if !ok || spec.ClientID == "" {
		return c, errors.New("invalid qoder credential")
	}
	if strings.TrimSpace(c.MachineID) == "" {
		return c, errors.New("invalid qoder credential")
	}
	if c.chatToken() == "" || strings.TrimSpace(c.UID) == "" {
		return c, errors.New("invalid qoder credential")
	}
	return c, nil
}

// chatToken is the token that signs model calls: the job token normally, and on
// a Qoder CN account that would not trade its device token, the device token.
func (c credential) chatToken() string {
	if c.DeviceChat || strings.TrimSpace(c.Access) == "" {
		return strings.TrimSpace(c.DeviceToken)
	}
	return strings.TrimSpace(c.Access)
}

// expiry is when the chat token runs out, zero when unknown.
func (c credential) expiry() time.Time {
	if c.Expires <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(c.Expires).UTC()
}

// label is the account name a card shows: a real email first, then the name, then
// the uid.
func (c credential) label() string {
	if e := realEmail(c.Email); e != "" {
		return e
	}
	if p := maskPhone(c.Phone); p != "" {
		return p
	}
	if phoneEmailPattern.MatchString(strings.TrimSpace(c.Email)) {
		if p := maskPhone(c.Name); p != "" {
			return p
		}
	}
	if n := strings.TrimSpace(c.Name); n != "" {
		return n
	}
	if e := strings.TrimSpace(c.Email); e != "" {
		return e
	}
	return strings.TrimSpace(c.UID)
}

// realEmail hides the synthetic phone address Qoder CN answers for phone
// accounts, which is an internal identifier rather than something to show.
func realEmail(email string) string {
	e := strings.TrimSpace(email)
	if e == "" || phoneEmailPattern.MatchString(e) {
		return ""
	}
	return e
}
