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
	"sync"
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

	// Serial is exclusive: scheduled RF poll and on-demand USB self-test share one mutex.
	var serialMu sync.Mutex

	runCycle := func() {
		serialMu.Lock()
		defer serialMu.Unlock()
		if err := pollOnce(vault, status, *serialPath, *baud, *perTimeout); err != nil {
			log.Printf("poll cycle error: %v", err)
		}
	}

	drainTests := func() {
		serialMu.Lock()
		defer serialMu.Unlock()
		processPendingUSBTests(*configDir, status, *serialPath, *baud)
	}

	runCycle()
	drainTests()
	if *once {
		return
	}

	lastPoll := time.Now()
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			drainTests()
			if time.Since(lastPoll) >= *interval {
				runCycle()
				lastPoll = time.Now()
			}
		case sig := <-stop:
			log.Printf("stopped (%v)", sig)
			return
		}
	}
}

type liveLink struct {
	info   companion.CompanionInfo
	port   companion.Port
	client *companion.Client
}

func openCompanion(serialPath string, baud int) (*liveLink, error) {
	info := companion.CompanionInfo{Port: serialPath, Baud: baud, LastOpen: time.Now().UTC()}
	port, err := companion.OpenSerial(serialPath, baud)
	if err != nil {
		info.OK = false
		info.LastError = err.Error()
		return &liveLink{info: info}, err
	}
	info.OK = true
	client := companion.NewClient(port, "corescope-poller")
	if err := client.Handshake(5 * time.Second); err != nil {
		_ = port.Close()
		info.OK = false
		info.LastError = "handshake: " + err.Error()
		return &liveLink{info: info}, fmt.Errorf("handshake: %w", err)
	}
	return &liveLink{info: info, port: port, client: client}, nil
}

func (l *liveLink) close() {
	if l != nil && l.port != nil {
		_ = l.port.Close()
		l.port = nil
		l.client = nil
	}
}

// reconnectAfterDisconnect waits for USB re-enumeration then opens a fresh link.
func reconnectAfterDisconnect(serialPath string, baud int, reason error) (*liveLink, error) {
	log.Printf("companion serial disconnected (%v) — waiting for USB re-enumerate then reconnecting", reason)
	time.Sleep(2 * time.Second)
	link, err := openCompanion(serialPath, baud)
	if err != nil {
		// One more try after a longer settle (some boards take 3–5s after brownout).
		time.Sleep(3 * time.Second)
		link, err = openCompanion(serialPath, baud)
	}
	return link, err
}

func pollOnce(vault *repeatervault.Store, status *companion.StatusStore, serialPath string, baud int, timeout time.Duration) error {
	link, err := openCompanion(serialPath, baud)
	if err != nil {
		_ = status.SetCompanion(link.info)
		return err
	}
	// Close whatever link is current at return (may be replaced after reconnect).
	defer func() { link.close() }()
	_ = status.SetCompanion(link.info)

	contactsTimeout := timeout
	if contactsTimeout < 30*time.Second {
		contactsTimeout = 30 * time.Second
	}
	contacts, reportedTotal, cerr := link.client.GetContacts(contactsTimeout)
	if cerr != nil {
		if companion.IsDisconnected(cerr) {
			link.close()
			link, err = reconnectAfterDisconnect(serialPath, baud, cerr)
			if err != nil {
				_ = status.SetCompanion(link.info)
				return err
			}
			_ = status.SetCompanion(link.info)
			contacts, reportedTotal, cerr = link.client.GetContacts(contactsTimeout)
		}
		if cerr != nil {
			log.Printf("get_contacts: FAIL %v (partial=%d reported=%d)", cerr, len(contacts), reportedTotal)
			link.info.LastError = "get_contacts: " + cerr.Error()
		} else {
			link.info.LastError = ""
		}
	} else {
		link.info.LastError = ""
	}
	link.info.ContactCount = len(contacts)
	_ = status.SetContacts(link.info, contacts)
	logContacts(contacts, reportedTotal)

	byKey := make(map[string]companion.Contact, len(contacts))
	for _, c := range contacts {
		byKey[strings.ToLower(c.PublicKey)] = c
	}
	contactsDirty := false

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
		seedName := strings.TrimSpace(r.Name)
		if seedName == "" {
			seedName = short(r.PublicKey)
		}

		ct, known := byKey[pkLower]
		// Flood (path unknown) brownouts weak USB; force zero-hop for poller-managed contacts.
		// Keep a learned direct path (1–64) when the companion has heard an advert.
		needZeroHop := !known || ct.OutPathLen == companion.OutPathUnknown
		if known {
			pathNote := "path unknown (will force zero-hop)"
			if ct.OutPathLen != companion.OutPathUnknown {
				pathNote = fmt.Sprintf("out_path_len=%d", ct.OutPathLen)
			}
			log.Printf("poll %s (%s): companion contact OK name=%q type=%s %s",
				short(r.PublicKey), r.Name, ct.Name, ct.TypeLabel, pathNote)
			if seedName == short(r.PublicKey) && ct.Name != "" {
				seedName = ct.Name
			}
		}

		if needZeroHop {
			action := "seeding"
			if known {
				action = "forcing zero-hop path on"
			}
			log.Printf("poll %s (%s): %s companion contact via CMD_ADD_UPDATE_CONTACT (out_path_len=0)",
				short(r.PublicKey), r.Name, action)
			if err := ensureContact(link, serialPath, baud, status, r.PublicKey, seedName); err != nil {
				snap.OK = false
				snap.Error = disconnectHint(err)
				snap.DurationMs = time.Since(start).Milliseconds()
				_ = status.Upsert(link.info, snap)
				if companion.IsDisconnected(err) {
					log.Printf("poll %s (%s): FAIL %s — aborting cycle", short(r.PublicKey), r.Name, snap.Error)
					return fmt.Errorf("companion reconnect failed: %w", err)
				}
				log.Printf("poll %s (%s): FAIL %v", short(r.PublicKey), r.Name, err)
				continue
			}
			seeded := companion.Contact{
				PublicKey:  r.PublicKey,
				Name:       seedName,
				Type:       companion.AdvTypeRepeater,
				TypeLabel:  "repeater",
				OutPathLen: companion.OutPathZeroHop,
			}
			byKey[pkLower] = seeded
			if !known {
				contacts = append(contacts, seeded)
			} else {
				for i := range contacts {
					if strings.EqualFold(contacts[i].PublicKey, r.PublicKey) {
						contacts[i] = seeded
						break
					}
				}
			}
			contactsDirty = true
			known = true
			log.Printf("poll %s (%s): companion contact ready (zero-hop) name=%q", short(r.PublicKey), r.Name, seedName)
		}

		pass, err := vault.DecryptAdminPassword(r.PublicKey)
		if err != nil {
			snap.OK = false
			snap.Error = "decrypt: " + err.Error()
			_ = status.Upsert(link.info, snap)
			continue
		}

		login, st, err := link.client.LoginAndStatus(r.PublicKey, pass, timeout)
		if companion.IsDisconnected(err) {
			link.close()
			var re *liveLink
			re, err = reconnectAfterDisconnect(serialPath, baud, err)
			if err != nil {
				snap.OK = false
				snap.Error = disconnectHint(err)
				snap.DurationMs = time.Since(start).Milliseconds()
				_ = status.Upsert(link.info, snap)
				log.Printf("poll %s (%s): FAIL %s — aborting cycle", short(r.PublicKey), r.Name, snap.Error)
				return fmt.Errorf("companion reconnect failed: %w", err)
			}
			link = re
			_ = status.SetCompanion(link.info)
			// Re-assert zero-hop after reconnect — contact table may still say flood.
			_ = link.client.AddOrUpdateContact(r.PublicKey, companion.AdvTypeRepeater, seedName, companion.OutPathZeroHop, 5*time.Second)
			log.Printf("poll %s (%s): retrying zero-hop login after reconnect", short(r.PublicKey), r.Name)
			login, st, err = link.client.LoginAndStatus(r.PublicKey, pass, timeout)
		}

		snap.DurationMs = time.Since(start).Milliseconds()
		if err != nil {
			snap.OK = false
			switch {
			case companion.IsDisconnected(err):
				snap.Error = disconnectHint(err)
				_ = status.Upsert(link.info, snap)
				log.Printf("poll %s (%s): FAIL %s — skipping to next repeater", short(r.PublicKey), r.Name, snap.Error)
				// Link is dead; reconnect so remaining repeaters can still be tried.
				link.close()
				re, rerr := reconnectAfterDisconnect(serialPath, baud, err)
				if rerr != nil {
					log.Printf("poll cycle: reconnect after skip failed: %v — aborting", rerr)
					return fmt.Errorf("companion reconnect failed: %w", rerr)
				}
				link = re
				_ = status.SetCompanion(link.info)
				continue
			case errors.Is(err, companion.ErrNotFound):
				snap.Error = companion.NotFoundHint(r.PublicKey, contacts)
			default:
				snap.Error = err.Error()
			}
			log.Printf("poll %s (%s): FAIL %s", short(r.PublicKey), r.Name, snap.Error)
			_ = login
		} else {
			snap.OK = true
			snap.IsAdmin = login.IsAdmin
			stats := st.Stats
			snap.Stats = &stats
			log.Printf("poll %s (%s): OK battery=%dmV uptime=%ds admin=%v",
				short(r.PublicKey), r.Name, stats.BatteryMv, stats.UptimeSecs, login.IsAdmin)
		}
		_ = status.Upsert(link.info, snap)
	}
	if contactsDirty {
		link.info.ContactCount = len(contacts)
		_ = status.SetContacts(link.info, contacts)
	}
	return nil
}

// processPendingUSBTests runs queued UI self-tests (Open + APP_START + GET_CONTACTS, no RF login).
func processPendingUSBTests(configDir string, status *companion.StatusStore, serialPath string, baud int) {
	pending, err := companion.ListPendingTestRequests(configDir)
	if err != nil || len(pending) == 0 {
		return
	}
	for _, path := range pending {
		req, err := companion.ReadTestRequest(path)
		if err != nil || req == nil || req.ID == "" {
			log.Printf("companion usb-test: skip bad request %s: %v", path, err)
			_ = os.Remove(path)
			continue
		}
		log.Printf("companion usb-test %s: starting (open → app_start → get_contacts)", req.ID)
		res := runUSBSelfTest(req, status, serialPath, baud)
		if err := companion.WriteTestResult(configDir, res); err != nil {
			log.Printf("companion usb-test %s: write result: %v", req.ID, err)
			continue
		}
		if res.OK {
			log.Printf("companion usb-test %s: OK contacts=%d duration=%dms", req.ID, res.ContactCount, res.DurationMs)
		} else {
			log.Printf("companion usb-test %s: FAIL %s duration=%dms", req.ID, res.Error, res.DurationMs)
		}
	}
}

// runUSBSelfTest opens the companion, handshakes, and lists contacts — no RF TX / login.
func runUSBSelfTest(req *companion.TestRequest, status *companion.StatusStore, serialPath string, baud int) companion.TestResult {
	start := time.Now()
	res := companion.TestResult{
		ID:          req.ID,
		RequestedAt: req.RequestedAt,
		Port:        serialPath,
		Baud:        baud,
		Steps:       []string{},
	}
	link, err := openCompanion(serialPath, baud)
	if err != nil {
		res.CompletedAt = time.Now().UTC()
		res.DurationMs = time.Since(start).Milliseconds()
		res.OK = false
		res.Error = link.info.LastError
		if res.Error == "" {
			res.Error = err.Error()
		}
		_ = status.SetCompanion(link.info)
		return res
	}
	defer link.close()
	res.Steps = append(res.Steps, "open", "app_start")
	_ = status.SetCompanion(link.info)

	contacts, reported, cerr := link.client.GetContacts(30 * time.Second)
	res.Steps = append(res.Steps, "get_contacts")
	link.info.ContactCount = len(contacts)
	if cerr != nil {
		link.info.LastError = "get_contacts: " + cerr.Error()
		link.info.OK = false
		_ = status.SetContacts(link.info, contacts)
		res.CompletedAt = time.Now().UTC()
		res.DurationMs = time.Since(start).Milliseconds()
		res.OK = false
		res.Error = link.info.LastError
		res.ContactCount = len(contacts)
		_ = reported
		return res
	}
	link.info.LastError = ""
	link.info.OK = true
	_ = status.SetContacts(link.info, contacts)
	res.CompletedAt = time.Now().UTC()
	res.DurationMs = time.Since(start).Milliseconds()
	res.OK = true
	res.ContactCount = len(contacts)
	return res
}

// ensureContact adds/updates a vaulted repeater as a zero-hop companion contact.
func ensureContact(link *liveLink, serialPath string, baud int, status *companion.StatusStore, pubKey, name string) error {
	err := link.client.AddOrUpdateContact(pubKey, companion.AdvTypeRepeater, name, companion.OutPathZeroHop, 5*time.Second)
	if err == nil {
		return nil
	}
	if !companion.IsDisconnected(err) {
		return err
	}
	link.close()
	re, rerr := reconnectAfterDisconnect(serialPath, baud, err)
	if rerr != nil {
		*link = *re
		return rerr
	}
	*link = *re
	_ = status.SetCompanion(link.info)
	return link.client.AddOrUpdateContact(pubKey, companion.AdvTypeRepeater, name, companion.OutPathZeroHop, 5*time.Second)
}

func disconnectHint(err error) string {
	base := "companion USB disconnected during RF TX"
	if err != nil {
		base = base + " (" + err.Error() + ")"
	}
	return base + " — serial dropped while handling CMD_SEND_LOGIN (before or during RESP_CODE_SENT). Commands match meshcore_py; poller uses zero-hop for seeded contacts. Check: only one process owns the tty, companion firmware is stable on RF TX, and (if multi-hop) the contact has a learned path — not only power."
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
