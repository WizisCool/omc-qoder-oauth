package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// maxStateLength is the host's OAuth state limit. The sealed blob has to fit in
// it, which is why the PKCE verifier is derived from the state key and the
// nonce instead of being stored.
const maxStateLength = 128

// loginState is the small amount of flow context CPA's opaque state carries
// between auth.login.start and auth.login.poll.
type loginState struct {
	Site      string
	Expires   int64
	Nonce     []byte // 16 bytes, sent to Qoder as a UUID
	MachineID []byte // 16 bytes, sent to Qoder as a UUID
}

var stateKeyMu sync.Mutex

func loadStateKey(authDir string) ([]byte, error) {
	if authDir == "" {
		return nil, errors.New("CPA auth directory unavailable")
	}
	stateKeyMu.Lock()
	defer stateKeyMu.Unlock()
	if err := os.MkdirAll(authDir, 0700); err != nil {
		return nil, errors.New("cannot create auth directory")
	}
	path := filepath.Join(authDir, ".qoder-oauth-state.key")
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, errors.New("invalid state encryption key")
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, errors.New("cannot read state encryption key")
	}
	key = make([]byte, 32)
	if _, err = io.ReadFull(rand.Reader, key); err != nil {
		return nil, errors.New("cannot create state encryption key")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		key, err = os.ReadFile(path)
		if err != nil || len(key) != 32 {
			return nil, errors.New("cannot read state encryption key")
		}
		return key, nil
	}
	if err != nil {
		return nil, errors.New("cannot persist state encryption key")
	}
	_, err = f.Write(key)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(path)
		return nil, errors.New("cannot persist state encryption key")
	}
	return key, nil
}

func newNonce() ([]byte, error) {
	b := make([]byte, 16)
	_, err := io.ReadFull(rand.Reader, b)
	return b, err
}

// deriveVerifier is the PKCE verifier for one login flow: 43 base64url
// characters from HMAC-SHA256 over the flow nonce under the install's state key.
// It is reproducible at poll time without lengthening the sealed state.
func deriveVerifier(key, nonce []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("qoder-pkce-v1\x00"))
	mac.Write(nonce)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// deriveChallenge is the S256 PKCE challenge for verifier.
func deriveChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// uuidString formats 16 bytes as the dashed UUID Qoder's client sends for its
// nonce and machine id.
func uuidString(b []byte) string {
	if len(b) != 16 {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func sealState(key []byte, s loginState) (string, error) {
	if len(s.Nonce) != 16 || len(s.MachineID) != 16 || s.Expires <= 0 {
		return "", errors.New("unsupported login flow state")
	}
	spec, ok := siteFor(s.Site)
	if !ok {
		return "", errors.New("unsupported login site")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", errors.New("cannot generate login state")
	}
	plain := make([]byte, 37)
	if spec.ID == siteQoderCN {
		plain[0] = 1
	}
	binary.BigEndian.PutUint32(plain[1:5], uint32(s.Expires))
	copy(plain[5:21], s.Nonce)
	copy(plain[21:37], s.MachineID)
	encoded := base64.RawURLEncoding.EncodeToString(append(append([]byte{}, nonce...), gcm.Seal(nil, nonce, plain, []byte("qoder-login-v1"))...))
	if len(encoded) > maxStateLength {
		return "", errors.New("login state exceeds the CPA state length limit")
	}
	return encoded, nil
}

func openState(key []byte, encoded string) (loginState, error) {
	var s loginState
	if len(encoded) > maxStateLength {
		return s, errors.New("invalid login state")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != 65 {
		return s, errors.New("invalid login state")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return s, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return s, err
	}
	nonce := raw[:12]
	p, err := gcm.Open(nil, nonce, raw[12:], []byte("qoder-login-v1"))
	if err != nil || len(p) != 37 || p[0]&^byte(1) != 0 {
		return s, errors.New("invalid login state")
	}
	s = loginState{
		Site:      siteQoder,
		Expires:   int64(binary.BigEndian.Uint32(p[1:5])),
		Nonce:     append([]byte(nil), p[5:21]...),
		MachineID: append([]byte(nil), p[21:37]...),
	}
	if p[0]&1 != 0 {
		s.Site = siteQoderCN
	}
	return s, nil
}
