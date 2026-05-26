package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"time"
)

// Session cookies are stateless: we sign `expiresAt|user` with a server
// secret derived from the configured panel password and verify it on every
// request. Rotating the password automatically invalidates every existing
// session — no extra wiring needed.
//
// Format (base64url, no padding):
//   exp:int64-bigendian | hmac-sha256(exp || user, key)
//
// We don't include the username in the cookie because we have exactly one
// user. Adding it later is trivial.

const (
	cookieName    = "rclient_sess"
	sessionMaxAge = 12 * time.Hour
)

// sessionKey returns the HMAC key to sign cookies with. It's derived from
// the panel password so that changing the password kills every existing
// cookie.
func (s *Server) sessionKey() []byte {
	h := sha256.New()
	h.Write([]byte("rclient session v1\x00"))
	h.Write([]byte(s.cfg.PanelPass))
	return h.Sum(nil)
}

// signSession produces a cookie value valid until `expires`.
func (s *Server) signSession(expires time.Time) string {
	var exp [8]byte
	binary.BigEndian.PutUint64(exp[:], uint64(expires.Unix()))
	mac := hmac.New(sha256.New, s.sessionKey())
	mac.Write(exp[:])
	tag := mac.Sum(nil)
	out := append(exp[:], tag...)
	return base64.RawURLEncoding.EncodeToString(out)
}

// verifySession returns nil if the cookie is well-formed, the signature is
// valid for the current key, and the cookie hasn't expired.
func (s *Server) verifySession(value string) error {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != 8+sha256.Size {
		return errors.New("malformed session cookie")
	}
	exp := raw[:8]
	tag := raw[8:]
	mac := hmac.New(sha256.New, s.sessionKey())
	mac.Write(exp)
	want := mac.Sum(nil)
	if !hmac.Equal(want, tag) {
		return errors.New("bad session signature")
	}
	if time.Now().Unix() > int64(binary.BigEndian.Uint64(exp)) {
		return errors.New("session expired")
	}
	return nil
}

// cookiePath is what we set on the session cookie so it's only sent for
// the panel routes, never with shell-output WS frames or anything else.
func (s *Server) cookiePath() string {
	if s.cfg.PanelPath == "" {
		return "/"
	}
	p := s.cfg.PanelPath
	if p[len(p)-1] != '/' {
		p += "/"
	}
	return p
}

// setSessionCookie issues a fresh cookie on a successful login.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request) {
	expires := time.Now().Add(sessionMaxAge)
	value := s.signSession(expires)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     s.cookiePath(),
		Expires:  expires,
		MaxAge:   int(sessionMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// clearSessionCookie wipes the cookie on logout.
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     s.cookiePath(),
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// hasValidSession returns true if the request carries a valid session cookie.
func (s *Server) hasValidSession(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	return s.verifySession(c.Value) == nil
}

// randomToken is a small helper for rare cases where we need short opaque
// strings, e.g. CSRF tokens. Currently unused; kept for future extension.
func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
