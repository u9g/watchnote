package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Keys derives purpose-specific keys from SECRET_KEY so one secret can't be
// replayed across uses (a session signature isn't a valid email-link signature).
type Keys struct {
	session, csrf, links, seal []byte
}

func newKeys(secret string) *Keys {
	derive := func(purpose string) []byte {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write([]byte("watchnote:" + purpose))
		return m.Sum(nil)
	}
	return &Keys{derive("session"), derive("csrf"), derive("links"), derive("seal")}
}

func mac(key []byte, msg string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func macEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// ---- sessions ----

const sessionTTL = 30 * 24 * time.Hour

func (k *Keys) SessionValue(uid int64, now time.Time) string {
	payload := strconv.FormatInt(uid, 10) + "." + strconv.FormatInt(now.Add(sessionTTL).Unix(), 10)
	return payload + "." + mac(k.session, payload)
}

func (k *Keys) ParseSession(v string, now time.Time) (int64, bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 || !macEqual(parts[2], mac(k.session, parts[0]+"."+parts[1])) {
		return 0, false
	}
	uid, err1 := strconv.ParseInt(parts[0], 10, 64)
	exp, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || now.Unix() > exp {
		return 0, false
	}
	return uid, true
}

func (k *Keys) CSRF(uid int64) string { return mac(k.csrf, strconv.FormatInt(uid, 10))[:32] }

// ---- signed email links ----

func (k *Keys) LinkSig(action string, watchID int64) string {
	return mac(k.links, action+":"+strconv.FormatInt(watchID, 10))[:32]
}

func (k *Keys) CheckLink(action string, watchID int64, sig string) bool {
	return macEqual(sig, k.LinkSig(action, watchID))
}

// ---- sealing GitHub tokens at rest ----

func (k *Keys) Seal(plain string) ([]byte, error) {
	block, err := aes.NewCipher(k.seal)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plain), nil), nil
}

func (k *Keys) Open(sealed []byte) (string, error) {
	block, err := aes.NewCipher(k.seal)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("sealed value too short")
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	return string(plain), err
}

// ---- random tokens ----

func randomToken(nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
