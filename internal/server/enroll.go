package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// enrollStore is a tiny on-disk registry of agent_id -> per-agent secret.
//
// First-contact policy: an agent may connect with any new agent_id and an
// empty Secret in Hello; the server generates a fresh secret, persists it,
// and ships it back via Welcome. From that point on the agent must present
// the same secret on every connect — otherwise the connection is rejected.
//
// This stops a holder of the shared bearer token from impersonating an
// already-enrolled box (because they don't have its per-agent secret).
//
// Persistence is a single JSON file. With three to a few dozen agents this
// is trivial and avoids dragging in a sqlite dependency.
type enrollStore struct {
	mu      sync.Mutex
	path    string
	secrets map[string]string // agent_id -> hex-encoded secret
}

func newEnrollStore(path string) (*enrollStore, error) {
	es := &enrollStore{path: path, secrets: map[string]string{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &es.secrets); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return es, nil
}

// authenticate accepts an agent_id and the secret it presented. Returns the
// secret to ship back to the agent in Welcome (non-empty only on first
// contact when we minted a fresh one), and an error if authentication
// failed.
func (s *enrollStore) authenticate(agentID, presented string) (issue string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, known := s.secrets[agentID]
	if known {
		// Existing agent — secret must match exactly. Constant-time so we
		// don't leak character positions through timing.
		if presented == "" {
			return "", errors.New("agent already enrolled, secret required")
		}
		if subtle.ConstantTimeCompare([]byte(stored), []byte(presented)) != 1 {
			return "", errors.New("agent secret mismatch")
		}
		return "", nil
	}

	// Brand-new agent_id. Mint a secret, persist it, hand it back so the
	// agent can store it and use it next time.
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	fresh := hex.EncodeToString(buf)
	s.secrets[agentID] = fresh
	if err := s.saveLocked(); err != nil {
		// Don't enrol if we can't persist — otherwise we'd issue a secret
		// that the server forgets and the agent can never re-auth.
		delete(s.secrets, agentID)
		return "", err
	}
	return fresh, nil
}

// forget removes an enrollment. Useful for an admin "revoke" command later.
// Currently unused but kept for completeness.
func (s *enrollStore) forget(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.secrets[agentID]; !ok {
		return nil
	}
	delete(s.secrets, agentID)
	return s.saveLocked()
}

// saveLocked writes the registry back to disk atomically. Caller must hold
// the lock.
func (s *enrollStore) saveLocked() error {
	data, err := json.MarshalIndent(s.secrets, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
