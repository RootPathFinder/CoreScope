package companion

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PollSnapshot is one repeater's latest poll result (passwords never stored).
type PollSnapshot struct {
	PublicKey  string         `json:"publicKey"`
	Name       string         `json:"name,omitempty"`
	PolledAt   time.Time      `json:"polledAt"`
	OK         bool           `json:"ok"`
	Error      string         `json:"error,omitempty"`
	IsAdmin    bool           `json:"isAdmin,omitempty"`
	Stats      *RepeaterStats `json:"stats,omitempty"`
	DurationMs int64          `json:"durationMs,omitempty"`
}

// StatusFile is the on-disk poll status document under data/.
type StatusFile struct {
	UpdatedAt    time.Time               `json:"updatedAt"`
	Companion    CompanionInfo           `json:"companion"`
	Contacts     []Contact               `json:"contacts,omitempty"`
	ContactsAt   time.Time               `json:"contactsAt,omitempty"`
	ContactCount int                     `json:"contactCount,omitempty"`
	Repeaters    map[string]PollSnapshot `json:"repeaters"`
}

// CompanionInfo describes the local USB companion link.
type CompanionInfo struct {
	Port         string    `json:"port"`
	Baud         int       `json:"baud"`
	OK           bool      `json:"ok"`
	LastError    string    `json:"lastError,omitempty"`
	LastOpen     time.Time `json:"lastOpen,omitempty"`
	ContactCount int       `json:"contactCount,omitempty"`
}

const statusFileName = "managed-repeater-status.json"

// StatusStore persists poll snapshots atomically (0600).
type StatusStore struct {
	path string
	mu   sync.Mutex
}

func StatusPath(configDir string) string {
	return filepath.Join(configDir, "data", statusFileName)
}

func OpenStatusStore(configDir string) *StatusStore {
	return &StatusStore{path: StatusPath(configDir)}
}

func (s *StatusStore) Path() string { return s.path }

func (s *StatusStore) Load() (StatusFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *StatusStore) loadLocked() (StatusFile, error) {
	var doc StatusFile
	doc.Repeaters = map[string]PollSnapshot{}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return doc, err
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, err
	}
	if doc.Repeaters == nil {
		doc.Repeaters = map[string]PollSnapshot{}
	}
	return doc, nil
}

func (s *StatusStore) Save(doc StatusFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(doc)
}

func (s *StatusStore) saveLocked(doc StatusFile) error {
	if doc.Repeaters == nil {
		doc.Repeaters = map[string]PollSnapshot{}
	}
	doc.UpdatedAt = time.Now().UTC()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *StatusStore) Upsert(companion CompanionInfo, snap PollSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.loadLocked()
	if err != nil {
		return err
	}
	doc.Companion = companion
	if doc.Repeaters == nil {
		doc.Repeaters = map[string]PollSnapshot{}
	}
	key := snap.PublicKey
	doc.Repeaters[key] = snap
	return s.saveLocked(doc)
}

func (s *StatusStore) SetCompanion(info CompanionInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.loadLocked()
	if err != nil {
		return err
	}
	doc.Companion = info
	return s.saveLocked(doc)
}

// SetContacts replaces the companion contact book snapshot used by the UI.
func (s *StatusStore) SetContacts(info CompanionInfo, contacts []Contact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.loadLocked()
	if err != nil {
		return err
	}
	info.ContactCount = len(contacts)
	doc.Companion = info
	doc.Contacts = append([]Contact(nil), contacts...)
	doc.ContactCount = len(contacts)
	doc.ContactsAt = time.Now().UTC()
	return s.saveLocked(doc)
}
