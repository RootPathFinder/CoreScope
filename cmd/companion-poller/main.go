// corescope-companion-poller polls managed repeaters via a local USB companion.
//
// Reads encrypted passwords from the managed-repeaters vault, performs RF
// admin login + status request over the companion serial protocol, and writes
// results to data/managed-repeater-status.json for the read-only server UI.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/meshcore-analyzer/companion"
	"github.com/meshcore-analyzer/repeatervault"
)

func main() {
	configDir := flag.String("config-dir", envOr("CORESCOPE_CONFIG_DIR", "/app"), "Directory containing data/ (vault + status)")
	serialPath := flag.String("serial", envOr("COMPANION_SERIAL", "/dev/ttyACM1"), "USB companion serial device")
	baud := flag.Int("baud", envInt("COMPANION_BAUD", 115200), "Serial baud rate")
	interval := flag.Duration("interval", envDuration("COMPANION_POLL_INTERVAL", 5*time.Minute), "Poll interval between full cycles")
	perTimeout := flag.Duration("timeout", envDuration("COMPANION_POLL_TIMEOUT", 20*time.Second), "Per-repeater RF timeout")
	once := flag.Bool("once", false, "Run a single poll cycle and exit")
	flag.Parse()

	apiKey := ""
	if cfg, err := loadAPIKey(*configDir); err == nil {
		apiKey = cfg
	}
	key, err := repeatervault.DeriveKey(os.Getenv("CORESCOPE_VAULT_KEY"), apiKey)
	if err != nil {
		log.Fatalf("vault key: %v (set CORESCOPE_VAULT_KEY or apiKey in config.json)", err)
	}
	vault, err := repeatervault.Open(*configDir, key)
	if err != nil {
		log.Fatalf("open vault: %v", err)
	}
	status := companion.OpenStatusStore(*configDir)
	log.Printf("companion-poller starting serial=%s baud=%d interval=%s vault=%s status=%s",
		*serialPath, *baud, interval.String(), vault.Path(), status.Path())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	runCycle := func() {
		if err := pollOnce(vault, status, *serialPath, *baud, *perTimeout); err != nil {
			log.Printf("poll cycle error: %v", err)
		}
	}

	runCycle()
	if *once {
		return
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			runCycle()
		case sig := <-stop:
			log.Printf("stopped (%v)", sig)
			return
		}
	}
}

func pollOnce(vault *repeatervault.Store, status *companion.StatusStore, serialPath string, baud int, timeout time.Duration) error {
	info := companion.CompanionInfo{Port: serialPath, Baud: baud, LastOpen: time.Now().UTC()}
	port, err := companion.OpenSerial(serialPath, baud)
	if err != nil {
		info.OK = false
		info.LastError = err.Error()
		_ = status.SetCompanion(info)
		return err
	}
	defer port.Close()
	info.OK = true
	_ = status.SetCompanion(info)

	client := companion.NewClient(port, "corescope-poller")
	if err := client.Handshake(5 * time.Second); err != nil {
		info.OK = false
		info.LastError = "handshake: " + err.Error()
		_ = status.SetCompanion(info)
		return fmt.Errorf("handshake: %w", err)
	}

	contactsTimeout := timeout
	if contactsTimeout < 30*time.Second {
		contactsTimeout = 30 * time.Second
	}
	contacts, reportedTotal, cerr := client.GetContacts(contactsTimeout)
	if cerr != nil {
		log.Printf("get_contacts: FAIL %v (partial=%d reported=%d)", cerr, len(contacts), reportedTotal)
		info.LastError = "get_contacts: " + cerr.Error()
		// Keep whatever we got so the UI still shows something useful.
	} else {
		info.LastError = ""
	}
	info.ContactCount = len(contacts)
	_ = status.SetContacts(info, contacts)
	logContacts(contacts, reportedTotal)

	byKey := make(map[string]companion.Contact, len(contacts))
	for _, c := range contacts {
		byKey[strings.ToLower(c.PublicKey)] = c
	}

	list, err := vault.List()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		log.Printf("no managed repeaters in vault (companion contacts=%d)", len(contacts))
		return nil
	}
	log.Printf("polling %d managed repeater(s); companion contacts=%d", len(list), len(contacts))

	for i, r := range list {
		if i > 0 {
			time.Sleep(2 * time.Second) // stagger RF load on single companion
		}
		start := time.Now()
		snap := companion.PollSnapshot{
			PublicKey: r.PublicKey,
			Name:      r.Name,
			PolledAt:  time.Now().UTC(),
		}
		pkLower := strings.ToLower(r.PublicKey)
		ct, known := byKey[pkLower]
		if known {
			pathNote := "path unknown"
			if ct.OutPathLen != 0xFF {
				pathNote = fmt.Sprintf("out_path_len=%d", ct.OutPathLen)
			}
			log.Printf("poll %s (%s): companion contact OK name=%q type=%s %s",
				short(r.PublicKey), r.Name, ct.Name, ct.TypeLabel, pathNote)
		} else {
			log.Printf("poll %s (%s): WARN not in companion contacts (%d known) — login will likely fail with ERR_CODE_NOT_FOUND",
				short(r.PublicKey), r.Name, len(contacts))
		}

		pass, err := vault.DecryptAdminPassword(r.PublicKey)
		if err != nil {
			snap.OK = false
			snap.Error = "decrypt: " + err.Error()
			_ = status.Upsert(info, snap)
			continue
		}
		login, st, err := client.LoginAndStatus(r.PublicKey, pass, timeout)
		snap.DurationMs = time.Since(start).Milliseconds()
		if err != nil {
			snap.OK = false
			if errors.Is(err, companion.ErrNotFound) {
				snap.Error = companion.NotFoundHint(r.PublicKey, contacts)
			} else {
				snap.Error = err.Error()
			}
			log.Printf("poll %s (%s): FAIL %s", short(r.PublicKey), r.Name, snap.Error)
			_ = login // keep partial for future enrichment
		} else {
			snap.OK = true
			snap.IsAdmin = login.IsAdmin
			stats := st.Stats
			snap.Stats = &stats
			log.Printf("poll %s (%s): OK battery=%dmV uptime=%ds admin=%v",
				short(r.PublicKey), r.Name, stats.BatteryMv, stats.UptimeSecs, login.IsAdmin)
		}
		_ = status.Upsert(info, snap)
	}
	return nil
}

func logContacts(contacts []companion.Contact, reported uint32) {
	if len(contacts) == 0 {
		log.Printf("companion contacts: 0 (reported total=%d) — managed logins will fail until the USB companion hears adverts", reported)
		return
	}
	parts := make([]string, 0, len(contacts))
	for _, c := range contacts {
		label := c.Name
		if label == "" {
			label = short(c.PublicKey)
		}
		parts = append(parts, fmt.Sprintf("%s[%s/%s]", label, short(c.PublicKey), c.TypeLabel))
	}
	log.Printf("companion contacts: %d (reported total=%d): %s", len(contacts), reported, strings.Join(parts, ", "))
}

func short(pk string) string {
	if len(pk) >= 8 {
		return pk[:8]
	}
	return pk
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		return def
	}
	return n
}

func envDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

type configAPIKey struct {
	APIKey string `json:"apiKey"`
}

func loadAPIKey(configDir string) (string, error) {
	candidates := []string{
		filepath.Join(configDir, "data", "config.json"),
		filepath.Join(configDir, "config.json"),
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var c configAPIKey
		if err := json.Unmarshal(data, &c); err != nil {
			continue
		}
		if c.APIKey != "" {
			return c.APIKey, nil
		}
	}
	return "", fmt.Errorf("apiKey not found")
}
