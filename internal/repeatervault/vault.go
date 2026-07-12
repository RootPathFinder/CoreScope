// Package repeatervault stores managed repeater credentials encrypted at rest.
//
// M1 of the active-telemetry feature: operators can register remote repeaters
// with admin passwords. Passwords are never returned by public list APIs.
// Decryption helpers exist for a future poller (M2) living in cmd/ingestor.
package repeatervault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	fileVersion = 1
	fileName    = "managed-repeaters.json"
	maxNameLen  = 128
	maxPassLen  = 256
)

var (
	ErrNotFound      = errors.New("managed repeater not found")
	ErrDuplicateKey  = errors.New("public key already registered")
	ErrInvalidKey    = errors.New("public key must be 64 hex characters")
	ErrInvalidName   = errors.New("name too long")
	ErrInvalidPass   = errors.New("admin password required")
	ErrNoVaultKey    = errors.New("no vault key: set CORESCOPE_VAULT_KEY or apiKey")
	ErrCorrupt       = errors.New("vault file corrupt or wrong key")
	pubkeyRe         = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Record is the on-disk shape. AdminPasswordEnc is nonce||ciphertext (base64).
type Record struct {
	ID               string    `json:"id"`
	PublicKey        string    `json:"publicKey"`
	Name             string    `json:"name,omitempty"`
	AdminPasswordEnc string    `json:"adminPasswordEnc"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

// PublicView is the API-safe projection (no ciphertext, no plaintext).
type PublicView struct {
	ID               string `json:"id"`
	PublicKey        string `json:"publicKey"`
	Name             string `json:"name,omitempty"`
	HasAdminPassword bool   `json:"hasAdminPassword"`
	CreatedAt        string `json:"createdAt"`
	UpdatedAt        string `json:"updatedAt"`
}

type fileDoc struct {
	Version   int      `json:"version"`
	Repeaters []Record `json:"repeaters"`
}

// Store is a mutex-guarded encrypted credential vault on disk.
type Store struct {
	mu   sync.Mutex
	path string
	key  [32]byte
}

// DeriveKey builds the AES-256 key.
// Preference: CORESCOPE_VAULT_KEY (64-char hex OR arbitrary passphrase) → apiKey.
func DeriveKey(vaultKeyEnv, apiKey string) ([32]byte, error) {
	var out [32]byte
	vaultKeyEnv = strings.TrimSpace(vaultKeyEnv)
	apiKey = strings.TrimSpace(apiKey)
	if vaultKeyEnv != "" {
		if len(vaultKeyEnv) == 64 {
			if b, err := hex.DecodeString(vaultKeyEnv); err == nil && len(b) == 32 {
				copy(out[:], b)
				return out, nil
			}
		}
		sum := sha256.Sum256([]byte("corescope-repeater-vault-v1|" + vaultKeyEnv))
		return sum, nil
	}
	if apiKey != "" {
		sum := sha256.Sum256([]byte("corescope-repeater-vault-v1|apiKey|" + apiKey))
		return sum, nil
	}
	return out, ErrNoVaultKey
}

// Open creates or loads a vault at configDir/data/managed-repeaters.json.
func Open(configDir string, key [32]byte) (*Store, error) {
	dataDir := filepath.Join(configDir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir vault dir: %w", err)
	}
	s := &Store{path: filepath.Join(dataDir, fileName), key: key}
	if _, err := os.Stat(s.path); errors.Is(err, os.ErrNotExist) {
		if err := s.saveLocked(fileDoc{Version: fileVersion, Repeaters: []Record{}}); err != nil {
			return nil, err
		}
		return s, nil
	} else if err != nil {
		return nil, err
	}
	// Touch-load to validate readability; decryption of entries happens on demand.
	if _, err := s.loadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path returns the vault file path.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// List returns API-safe views (no secrets).
func (s *Store) List() ([]PublicView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.loadLocked()
	if err != nil {
		return nil, err
	}
	out := make([]PublicView, 0, len(doc.Repeaters))
	for _, r := range doc.Repeaters {
		out = append(out, toPublic(r))
	}
	return out, nil
}

// Add registers a repeater. password is required.
func (s *Store) Add(publicKey, name, password string) (PublicView, error) {
	pk, err := NormalizePublicKey(publicKey)
	if err != nil {
		return PublicView{}, err
	}
	name, err = NormalizeName(name)
	if err != nil {
		return PublicView{}, err
	}
	if strings.TrimSpace(password) == "" || len(password) > maxPassLen {
		return PublicView{}, ErrInvalidPass
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.loadLocked()
	if err != nil {
		return PublicView{}, err
	}
	for _, r := range doc.Repeaters {
		if r.PublicKey == pk {
			return PublicView{}, ErrDuplicateKey
		}
	}
	enc, err := s.encrypt(password)
	if err != nil {
		return PublicView{}, err
	}
	now := time.Now().UTC()
	rec := Record{
		ID:               newID(),
		PublicKey:        pk,
		Name:             name,
		AdminPasswordEnc: enc,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	doc.Repeaters = append(doc.Repeaters, rec)
	if err := s.saveLocked(doc); err != nil {
		return PublicView{}, err
	}
	return toPublic(rec), nil
}

// Update changes name and/or password. Empty password keeps the existing one.
func (s *Store) Update(id, name, password string, setName, setPassword bool) (PublicView, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return PublicView{}, ErrNotFound
	}
	var (
		normName string
		err      error
	)
	if setName {
		normName, err = NormalizeName(name)
		if err != nil {
			return PublicView{}, err
		}
	}
	if setPassword {
		if strings.TrimSpace(password) == "" || len(password) > maxPassLen {
			return PublicView{}, ErrInvalidPass
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.loadLocked()
	if err != nil {
		return PublicView{}, err
	}
	idx := -1
	for i, r := range doc.Repeaters {
		if r.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return PublicView{}, ErrNotFound
	}
	rec := doc.Repeaters[idx]
	if setName {
		rec.Name = normName
	}
	if setPassword {
		enc, err := s.encrypt(password)
		if err != nil {
			return PublicView{}, err
		}
		rec.AdminPasswordEnc = enc
	}
	rec.UpdatedAt = time.Now().UTC()
	doc.Repeaters[idx] = rec
	if err := s.saveLocked(doc); err != nil {
		return PublicView{}, err
	}
	return toPublic(rec), nil
}

// Delete removes a managed repeater by id.
func (s *Store) Delete(id string) error {
	id = strings.TrimSpace(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.loadLocked()
	if err != nil {
		return err
	}
	kept := doc.Repeaters[:0]
	found := false
	for _, r := range doc.Repeaters {
		if r.ID == id {
			found = true
			continue
		}
		kept = append(kept, r)
	}
	if !found {
		return ErrNotFound
	}
	doc.Repeaters = kept
	return s.saveLocked(doc)
}

// DecryptAdminPassword returns the plaintext admin password for a public key.
// Intended for the future poller (M2); not exposed over HTTP.
func (s *Store) DecryptAdminPassword(publicKey string) (string, error) {
	pk, err := NormalizePublicKey(publicKey)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.loadLocked()
	if err != nil {
		return "", err
	}
	for _, r := range doc.Repeaters {
		if r.PublicKey == pk {
			return s.decrypt(r.AdminPasswordEnc)
		}
	}
	return "", ErrNotFound
}

// NormalizePublicKey lowercases and validates a 32-byte hex pubkey.
func NormalizePublicKey(pk string) (string, error) {
	pk = strings.ToLower(strings.TrimSpace(pk))
	if strings.HasPrefix(pk, "0x") {
		pk = pk[2:]
	}
	if !pubkeyRe.MatchString(pk) {
		return "", ErrInvalidKey
	}
	return pk, nil
}

// NormalizeName trims and bounds the display name.
func NormalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if len(name) > maxNameLen {
		return "", ErrInvalidName
	}
	return name, nil
}

func toPublic(r Record) PublicView {
	return PublicView{
		ID:               r.ID,
		PublicKey:        r.PublicKey,
		Name:             r.Name,
		HasAdminPassword: r.AdminPasswordEnc != "",
		CreatedAt:        r.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:        r.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func (s *Store) encrypt(plain string) (string, error) {
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (s *Store) decrypt(enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", ErrCorrupt
	}
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", ErrCorrupt
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", ErrCorrupt
	}
	return string(pt), nil
}

func (s *Store) loadLocked() (fileDoc, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fileDoc{}, err
	}
	var doc fileDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return fileDoc{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if doc.Repeaters == nil {
		doc.Repeaters = []Record{}
	}
	if doc.Version == 0 {
		doc.Version = fileVersion
	}
	return doc, nil
}

func (s *Store) saveLocked(doc fileDoc) error {
	doc.Version = fileVersion
	if doc.Repeaters == nil {
		doc.Repeaters = []Record{}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Extremely unlikely; fall back to timestamp-ish hex.
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
