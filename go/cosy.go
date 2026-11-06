package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
)

// qoderRSAPublicKey is the 1024-bit key Qoder's client embeds: it wraps the
// per-request AES key of the COSY Authorization header.
const qoderRSAPublicKey = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

// cosyAlphabet is the client's shuffled base64 alphabet ('$' pads).
const cosyAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"

// cosyUser is the account the envelope carries.
type cosyUser struct {
	UID       string
	Name      string
	Email     string
	Access    string
	MachineID string
}

func cosyUserFrom(c credential) cosyUser {
	return cosyUser{UID: c.UID, Name: c.Name, Email: c.Email, Access: c.chatToken(), MachineID: c.MachineID}
}

// cosyMachineOS names the machine as Qoder's desktop client does:
// x86_64_linux, aarch64_darwin, x86_64_win32.
func cosyMachineOS() string {
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}
	return arch + "_" + runtime.GOOS
}

// cosyMachineHostname is the Cosy-MachineHostname of a chat request: the
// hostname as printable ASCII, at most 96 characters, or a sanitized form with
// a hash of the original.
func cosyMachineHostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return sanitizeHostname(name)
}

func sanitizeHostname(name string) string {
	v := strings.TrimSpace(name)
	if v == "" {
		return ""
	}
	safe := func(s string) bool {
		for i := 0; i < len(s); i++ {
			if s[i] < 0x21 || s[i] > 0x7e {
				return false
			}
		}
		return true
	}
	cap96 := func(s string) string {
		if len(s) <= 96 {
			return s
		}
		head := strings.TrimRight(s[:96-8-1], "- ")
		sum := sha256.Sum256([]byte(s))
		if head == "" {
			return "unknown-" + hex.EncodeToString(sum[:4])
		}
		return head + "-" + hex.EncodeToString(sum[:4])
	}
	if safe(v) {
		return cap96(v)
	}
	var b strings.Builder
	dash := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c >= 0x21 && c <= 0x7e {
			b.WriteByte(c)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	cleaned := strings.Trim(b.String(), "-")
	sum := sha256.Sum256([]byte(v))
	if cleaned == "" {
		return cap96("unknown-" + hex.EncodeToString(sum[:4]))
	}
	return cap96(cleaned + "-" + hex.EncodeToString(sum[:4]))
}

func uuidText() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return uuidString(b), nil
}

func rsaKey() (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(qoderRSAPublicKey))
	if block == nil {
		return nil, errors.New("Qoder signing key is unavailable")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("Qoder signing key is unavailable")
	}
	pub, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("Qoder signing key is unavailable")
	}
	return pub, nil
}

// userBlob is the account as the envelope carries it: AES-128-CBC under a fresh
// key that is its own IV, the key wrapped with Qoder's RSA key.
func userBlob(u cosyUser, pub *rsa.PublicKey) (info, wrapped string, err error) {
	raw, err := json.Marshal(map[string]string{
		"uid":                  u.UID,
		"aid":                  "",
		"name":                 u.Name,
		"email":                u.Email,
		"security_oauth_token": u.Access,
	})
	if err != nil {
		return "", "", errors.New("cannot encode Qoder account envelope")
	}
	seed := make([]byte, 8)
	if _, err = io.ReadFull(rand.Reader, seed); err != nil {
		return "", "", errors.New("cannot create Qoder envelope key")
	}
	key := []byte(hex.EncodeToString(seed)) // 16 ASCII bytes
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", errors.New("cannot create Qoder envelope cipher")
	}
	padded := pkcs7Pad(raw, aes.BlockSize)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(out, padded)
	encrypted, err := rsa.EncryptPKCS1v15(rand.Reader, pub, key)
	if err != nil {
		return "", "", errors.New("cannot wrap Qoder envelope key")
	}
	return base64.StdEncoding.EncodeToString(out), base64.StdEncoding.EncodeToString(encrypted), nil
}

func pkcs7Pad(b []byte, size int) []byte {
	n := size - len(b)%size
	return append(append([]byte{}, b...), bytes.Repeat([]byte{byte(n)}, n)...)
}

// cosyPath is the path the signature covers: the request path without the
// leading "/algo" segment, query excluded.
func cosyPath(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return ""
	}
	path := u.Path
	if strings.HasPrefix(path, "/algo") {
		path = path[len("/algo"):]
	}
	return path
}

// cosyHeaders signs one call to target whose body, in its wire form, is body.
func cosyHeaders(target string, u cosyUser, body string, spec siteSpec, version string, ts int64) (http.Header, error) {
	if strings.TrimSpace(u.MachineID) == "" {
		return nil, errors.New("the Qoder account has no machine id; sign in again")
	}
	pub, err := rsaKey()
	if err != nil {
		return nil, err
	}
	info, wrapped, err := userBlob(u, pub)
	if err != nil {
		return nil, err
	}
	requestID, err := uuidText()
	if err != nil {
		return nil, errors.New("cannot create Qoder request id")
	}
	payloadJSON, err := json.Marshal(map[string]string{
		"version":     "v1",
		"requestId":   requestID,
		"info":        info,
		"cosyVersion": version,
		"ideVersion":  "",
	})
	if err != nil {
		return nil, errors.New("cannot encode Qoder signature payload")
	}
	payload := base64.StdEncoding.EncodeToString(payloadJSON)
	sum := md5.Sum([]byte(fmt.Sprintf("%s\n%s\n%d\n%s\n%s", payload, wrapped, ts, body, cosyPath(target))))
	sig := hex.EncodeToString(sum[:])
	m := u.MachineID
	return http.Header{
		"Accept":                {"application/json"},
		"Accept-Encoding":       {"identity"},
		"Content-Type":          {"application/json"},
		"Authorization":         {"Bearer COSY." + payload + "." + sig},
		"Cosy-Business-Product": {"app"},
		"Cosy-Business-Type":    {"agent"},
		"Cosy-ClientIp":         {m},
		"Cosy-ClientType":       {"10"},
		"Cosy-Data-Policy":      {"disagree"},
		"Cosy-Date":             {fmt.Sprintf("%d", ts)},
		"Cosy-Key":              {wrapped},
		"Cosy-MachineId":        {m},
		"Cosy-MachineToken":     {m},
		"Cosy-MachineType":      {"5"},
		"Cosy-MachineOS":        {cosyMachineOS()},
		"Cosy-Scene":            {"app"},
		"Cosy-User":             {u.UID},
		"Cosy-Version":          {version},
		"Login-Version":         {"v2"},
	}, nil
}

// chatHeaders are the headers of an agent_chat_generation call, as Qoder's
// client builds them: no client IP or machine token, the hostname, and the
// chosen model's key and source.
func chatHeaders(target string, u cosyUser, body string, spec siteSpec, version string, m qoderModel, ts int64) (http.Header, error) {
	h, err := cosyHeaders(target, u, body, spec, version, ts)
	if err != nil {
		return nil, err
	}
	// The envelope's header names keep their exact casing on the wire, so the
	// map keys are deleted directly rather than through http.Header.Del, which
	// would look for the canonicalized spelling.
	delete(h, "Cosy-ClientIp")
	delete(h, "Cosy-MachineToken")
	delete(h, "Cosy-MachineType")
	if host := cosyMachineHostname(); host != "" {
		h.Set("Cosy-MachineHostname", host)
	}
	h.Set("Accept", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Model-Key", m.Key)
	h.Set("X-Model-Source", firstNonEmpty(m.Source, "system"))
	return h, nil
}

// segmentEncode is base64 through the client's shuffled alphabet, '$' padding.
func segmentEncode(in []byte) string {
	var b strings.Builder
	acc, bits := uint32(0), 0
	for _, v := range in {
		acc = (acc<<8 | uint32(v)) & 0xffffff
		bits += 8
		for bits >= 6 {
			bits -= 6
			b.WriteByte(cosyAlphabet[(acc>>uint(bits))&63])
		}
	}
	if bits > 0 {
		b.WriteByte(cosyAlphabet[(acc<<uint(6-bits))&63])
	}
	for b.Len()%4 != 0 {
		b.WriteByte('$')
	}
	return b.String()
}

func swapThirds(s string) string {
	t := len(s) / 3
	if t == 0 {
		return s
	}
	return s[len(s)-t:] + s[t:len(s)-t] + s[:t]
}

// encodeBody is the wire codec of agent_chat_generation: the JSON body in the
// client's own base64 alphabet, then its first and last thirds swapped.
func encodeBody(plain []byte) string {
	return swapThirds(segmentEncode(plain))
}

// decodeBody reverses encodeBody. It is used by tests and diagnostics.
func decodeBody(wire string) ([]byte, error) {
	s := swapThirds(wire)
	var out []byte
	for g := 0; g+4 <= len(s); g += 4 {
		var values []int
		for i := 0; i < 4; i++ {
			c := s[g+i]
			if c == '$' {
				continue
			}
			at := strings.IndexByte(cosyAlphabet, c)
			if at < 0 {
				return nil, errors.New("invalid encoded body")
			}
			values = append(values, at)
		}
		x := 0
		for _, v := range values {
			x = x*64 + v
		}
		n := 6 * len(values) / 8
		for k := 0; k < n; k++ {
			out = append(out, byte((x>>uint(6*len(values)-8*(k+1)))&255))
		}
	}
	return out, nil
}
