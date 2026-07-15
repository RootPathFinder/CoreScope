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

// Deep-diagnostic knobs, set from env in main().
var (
	verboseSerial bool   // COMPANION_VERBOSE: byte-level serial trace
	onlyPubKey    string // COMPANION_ONLY_PUBKEY: poll only this repeater (prefix match)
)

func main() {
	configDir := flag.String("config-dir", envOr("CORESCOPE_CONFIG_DIR", "/app"), "Directory containing data/ (vault + status)")
	serialPath := flag.String("serial", envOr("COMPANION_SERIAL", "/dev/ttyACM1"), "USB companion serial device")
	baud := flag.Int("baud", envInt("COMPANION_BAUD", 115200), "Serial baud rate")
	interval := flag.Duration("interval", envDuration("COMPANION_POLL_INTERVAL", 5*time.Minute), "Poll interval between full cycles")
	perTimeout := flag.Duration("timeout", envDuration("COMPANION_POLL_TIMEOUT", 20*time.Second), "Per-repeater RF timeout")
	once := flag.Bool("once", false, "Run a single poll cycle and exit")
	flag.Parse()

	// Deep-diagnostic knobs (env-gated; safe to leave unset):
	//   COMPANION_VERBOSE=1            → byte-level serial trace + errno classification
	//   COMPANION_ONLY_PUBKEY=<hex>    → poll only the matching repeater (prefix ok)
	verboseSerial = envBool("COMPANION_VERBOSE", false)
	onlyPubKey = strings.ToLower(strings.TrimSpace(os.Getenv("COMPANION_ONLY_PUBKEY")))
	if verboseSerial {
		log.Printf("verbose serial tracing ENABLED (COMPANION_VERBOSE=1)")
	}
	if onlyPubKey != "" {
		log.Printf("single-repeater monitor ENABLED (COMPANION_ONLY_PUBKEY=%s) — other repeaters skipped", onlyPubKey)
	}

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

	drainConfig := func() {
		serialMu.Lock()
		defer serialMu.Unlock()
		processPendingConfigRequests(*configDir, status, *serialPath, *baud)
	}

	runCycle()
	drainConfig()
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
			drainConfig()
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
	if verboseSerial {
		port = companion.NewTracePort(port, filepath.Base(serialPath), log.Printf)
	}
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
	if onlyPubKey != "" {
		filtered := list[:0:0]
		for _, r := range list {
			if strings.HasPrefix(strings.ToLower(r.PublicKey), onlyPubKey) {
				filtered = append(filtered, r)
			}
		}
		list = filtered
		if len(list) == 0 {
			log.Printf("COMPANION_ONLY_PUBKEY=%s matched no managed repeater — nothing to poll", onlyPubKey)
			return nil
		}
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

		// Baseline the companion uptime right before the secure request so a
		// disconnect can be proven to be a real MCU reboot (uptime → ~0) rather
		// than a USB CDC hiccup (uptime keeps climbing). Best-effort: 0 if the
		// firmware predates CMD_GET_STATS.
		uptimeBefore := readUptime(link)

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
			logRebootEvidence(link, uptimeBefore, fmt.Sprintf("poll %s (%s)", short(r.PublicKey), r.Name))
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
				logRebootEvidence(link, uptimeBefore, fmt.Sprintf("poll %s (%s)", short(r.PublicKey), r.Name))
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

// processPendingUSBTests runs queued UI self-tests. TestModeUSB is read-only
// (open → app_start → device_query → battery → get_contacts); TestModeAdvert
// adds a single zero-hop self-advert to exercise RF TX in isolation.
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
		mode := companion.NormalizeTestMode(req.Mode)
		log.Printf("companion usb-test %s: starting mode=%s", req.ID, mode)
		res := runUSBSelfTest(req, status, serialPath, baud)
		if err := companion.WriteTestResult(configDir, res); err != nil {
			log.Printf("companion usb-test %s: write result: %v", req.ID, err)
			continue
		}
		if res.OK {
			log.Printf("companion usb-test %s: OK mode=%s contacts=%d advertSent=%v duration=%dms",
				req.ID, res.Mode, res.ContactCount, res.AdvertSent, res.DurationMs)
		} else {
			log.Printf("companion usb-test %s: FAIL mode=%s %s duration=%dms", req.ID, res.Mode, res.Error, res.DurationMs)
		}
	}
}

// runUSBSelfTest opens the companion and runs a sequence of read-only queries
// (proving the protocol + real device responses), optionally followed by a
// zero-hop self-advert (RF TX) in TestModeAdvert. Every step records the actual
// device reply so the UI never assumes success.
func runUSBSelfTest(req *companion.TestRequest, status *companion.StatusStore, serialPath string, baud int) companion.TestResult {
	start := time.Now()
	mode := companion.NormalizeTestMode(req.Mode)
	res := companion.TestResult{
		ID:          req.ID,
		RequestedAt: req.RequestedAt,
		Mode:        mode,
		Port:        serialPath,
		Baud:        baud,
	}
	finish := func() companion.TestResult {
		res.CompletedAt = time.Now().UTC()
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	}

	link, err := openCompanion(serialPath, baud)
	if err != nil {
		detail := link.info.LastError
		if detail == "" {
			detail = err.Error()
		}
		res.AddStep("open", false, detail)
		res.OK = false
		res.Error = detail
		_ = status.SetCompanion(link.info)
		return finish()
	}
	defer link.close()
	res.AddStep("open", true, fmt.Sprintf("%s @ %d baud", serialPath, baud))
	_ = status.SetCompanion(link.info)

	// app_start → self-info (device's own pubkey / name / radio params).
	self, serr := link.client.AppStartInfo(5 * time.Second)
	if serr != nil {
		res.AddStep("app_start", false, serr.Error())
		link.info.OK = false
		link.info.LastError = "app_start: " + serr.Error()
		_ = status.SetCompanion(link.info)
		res.OK = false
		res.Error = "app_start: " + serr.Error()
		return finish()
	}
	res.Self = &self
	res.AddStep("app_start", true, fmt.Sprintf("node=%q pubkey=%s tx=%ddBm", self.NodeName, short(self.PublicKey), self.TxPower))

	// device_query → firmware version/build (best-effort; older firmware may skip).
	if dev, derr := link.client.QueryDeviceInfo(5 * time.Second); derr != nil {
		if companion.IsDisconnected(derr) {
			res.AddStep("device_query", false, derr.Error())
			res.OK = false
			res.Error = "device_query: " + derr.Error()
			return finish()
		}
		res.AddStep("device_query", false, derr.Error())
	} else {
		res.Device = &dev
		res.AddStep("device_query", true, fmt.Sprintf("fw=%s build=%s ver_code=%d", dev.FirmwareVersion, dev.FirmwareBuild, dev.FirmwareVerCode))
	}

	// battery + storage (best-effort).
	if batt, berr := link.client.GetBattStorage(5 * time.Second); berr != nil {
		if companion.IsDisconnected(berr) {
			res.AddStep("battery", false, berr.Error())
			res.OK = false
			res.Error = "battery: " + berr.Error()
			return finish()
		}
		res.AddStep("battery", false, berr.Error())
	} else {
		res.Battery = &batt
		res.AddStep("battery", true, fmt.Sprintf("%dmV storage=%d/%dkB", batt.BatteryMv, batt.StorageUsedKb, batt.StorageTotKb))
	}

	// core_stats → uptime (best-effort; v8+ firmware). Baseline for reset proof.
	var uptimeBefore uint32
	if cs, cerr := link.client.GetCoreStats(5 * time.Second); cerr != nil {
		res.AddStep("core_stats", false, cerr.Error())
	} else {
		uptimeBefore = cs.UptimeSecs
		res.UptimeSecs = cs.UptimeSecs
		res.AddStep("core_stats", true, fmt.Sprintf("uptime=%ds batt=%dmV errFlags=0x%04x queue=%d", cs.UptimeSecs, cs.BatteryMv, cs.ErrFlags, cs.QueueLen))
	}

	// get_contacts (required; proves streamed multi-frame protocol works).
	contacts, reported, cerr := link.client.GetContacts(30 * time.Second)
	link.info.ContactCount = len(contacts)
	res.ContactCount = len(contacts)
	if cerr != nil {
		res.AddStep("get_contacts", false, cerr.Error())
		link.info.LastError = "get_contacts: " + cerr.Error()
		link.info.OK = false
		_ = status.SetContacts(link.info, contacts)
		res.OK = false
		res.Error = "get_contacts: " + cerr.Error()
		return finish()
	}
	_ = reported
	res.AddStep("get_contacts", true, fmt.Sprintf("%d contact(s)", len(contacts)))
	link.info.LastError = ""
	link.info.OK = true
	_ = status.SetContacts(link.info, contacts)

	// Optional RF TX: a single zero-hop self-advert to test transmit in isolation.
	// RESP_CODE_OK only means the command was accepted; the transmit runs after.
	// So we wait out the airtime and re-probe the device — otherwise a TX-triggered
	// reset (the exact thing login hits) would go unobserved and we'd falsely
	// report "advert works".
	if mode == companion.TestModeAdvert {
		const advertSettle = 4 * time.Second
		alive, probeErr, aerr := link.client.SelfAdvertAndProbe(false, advertSettle, 5*time.Second)
		if aerr != nil {
			res.AddStep("self_advert", false, aerr.Error())
			link.info.OK = false
			link.info.LastError = "self_advert: " + aerr.Error()
			_ = status.SetCompanion(link.info)
			res.OK = false
			if companion.IsDisconnected(aerr) {
				res.Error = "companion dropped while accepting a bare zero-hop self-advert (" + aerr.Error() + ") — the link failed before the transmit even ran, so this is a serial/handshake fault, not RF airtime."
			} else {
				res.Error = "self_advert: " + aerr.Error()
			}
			return finish()
		}
		res.AdvertSent = true
		res.AddStep("self_advert", true, "zero-hop advert accepted (RESP_CODE_OK) — command queued, RF transmit pending")

		if !alive {
			res.AddStep("post_advert_alive", false, probeErr.Error())
			link.info.OK = false
			link.info.LastError = "post_advert: " + probeErr.Error()
			_ = status.SetCompanion(link.info)
			res.OK = false
			res.Error = "companion reset AFTER a bare zero-hop self-advert RF transmit (" + probeErr.Error() + ") — it accepted the command (RESP_CODE_OK) but dropped the USB link once the transmit physically ran. This is RF-TX-triggered (PA current draw or firmware TX handling), NOT login-specific: a plain advert with no login/password/contact reproduces it. Try lowering TX power via radio config, a powered USB hub / shorter cable, or updating companion firmware."
			return finish()
		}
		detail := "device still responsive after advert RF transmit — a bare RF TX does NOT reset this device (points at the login/secure-request path, not raw TX)"
		if probeErr != nil {
			detail = "device responsive after advert RF transmit (liveness probe note: " + probeErr.Error() + ")"
		}
		res.AddStep("post_advert_alive", true, detail)

		// Uptime proof: if the MCU had silently rebooted during the advert TX,
		// uptime would run backwards. Confirms the "survived TX" claim with a
		// hard number rather than just a responsive probe.
		if uptimeBefore > 0 {
			uptimeAfter := readUptime(link)
			switch {
			case uptimeAfter == 0:
				res.AddStep("uptime_check", true, fmt.Sprintf("uptime unreadable after advert (was %ds) — probe alive, but no uptime confirmation", uptimeBefore))
			case companion.UptimeIndicatesReboot(uptimeBefore, uptimeAfter):
				res.AddStep("uptime_check", false, fmt.Sprintf("REBOOT during advert TX — uptime %ds → %ds (ran backwards)", uptimeBefore, uptimeAfter))
			default:
				res.AddStep("uptime_check", true, fmt.Sprintf("no reboot — uptime %ds → %ds (kept climbing across the advert TX)", uptimeBefore, uptimeAfter))
			}
		}
	}

	res.OK = true
	return finish()
}

// processPendingConfigRequests applies queued radio-config changes from the UI
// (CMD_SET_RADIO_PARAMS / CMD_SET_RADIO_TX_POWER), then re-reads self-info to
// prove the device accepted the new settings.
func processPendingConfigRequests(configDir string, status *companion.StatusStore, serialPath string, baud int) {
	pending, err := companion.ListPendingConfigRequests(configDir)
	if err != nil || len(pending) == 0 {
		return
	}
	for _, path := range pending {
		req, err := companion.ReadConfigRequest(path)
		if err != nil || req == nil || req.ID == "" {
			log.Printf("companion config: skip bad request %s: %v", path, err)
			_ = os.Remove(path)
			continue
		}
		log.Printf("companion config %s: applying region=%q radio=%v txPower=%v", req.ID, req.Region, req.Radio != nil, req.TxPowerDbm != nil)
		res := runConfigApply(req, status, serialPath, baud)
		if err := companion.WriteConfigResult(configDir, res); err != nil {
			log.Printf("companion config %s: write result: %v", req.ID, err)
			continue
		}
		if res.OK {
			log.Printf("companion config %s: OK duration=%dms", req.ID, res.DurationMs)
		} else {
			log.Printf("companion config %s: FAIL %s duration=%dms", req.ID, res.Error, res.DurationMs)
		}
	}
}

// runConfigApply opens the companion, applies the requested radio params / tx
// power, and re-reads self-info so the UI can confirm the change stuck.
func runConfigApply(req *companion.ConfigRequest, status *companion.StatusStore, serialPath string, baud int) companion.ConfigResult {
	start := time.Now()
	res := companion.ConfigResult{
		ID:          req.ID,
		RequestedAt: req.RequestedAt,
		Region:      req.Region,
		Applied:     req.Radio,
		TxPowerDbm:  req.TxPowerDbm,
	}
	finish := func() companion.ConfigResult {
		res.CompletedAt = time.Now().UTC()
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	}

	if err := req.Validate(); err != nil {
		res.AddStep("validate", false, err.Error())
		res.OK = false
		res.Error = err.Error()
		return finish()
	}

	link, err := openCompanion(serialPath, baud)
	if err != nil {
		detail := link.info.LastError
		if detail == "" {
			detail = err.Error()
		}
		res.AddStep("open", false, detail)
		res.OK = false
		res.Error = detail
		_ = status.SetCompanion(link.info)
		return finish()
	}
	defer link.close()
	res.AddStep("open", true, fmt.Sprintf("%s @ %d baud", serialPath, baud))
	_ = status.SetCompanion(link.info)

	if req.Radio != nil {
		if err := link.client.SetRadioParams(*req.Radio, 8*time.Second); err != nil {
			res.AddStep("set_radio_params", false, err.Error())
			res.OK = false
			res.Error = "set_radio_params: " + err.Error()
			return finish()
		}
		res.AddStep("set_radio_params", true, fmt.Sprintf("%.3f MHz bw=%.1f kHz sf=%d cr=%d",
			float64(req.Radio.FreqKHz)/1000.0, float64(req.Radio.BandwidthHz)/1000.0, req.Radio.SF, req.Radio.CR))
	}

	if req.TxPowerDbm != nil {
		if err := link.client.SetTxPower(*req.TxPowerDbm, 5*time.Second); err != nil {
			res.AddStep("set_tx_power", false, err.Error())
			res.OK = false
			res.Error = "set_tx_power: " + err.Error()
			return finish()
		}
		res.AddStep("set_tx_power", true, fmt.Sprintf("%d dBm", *req.TxPowerDbm))
	}

	// Re-read self-info to prove the device applied the change.
	if self, serr := link.client.AppStartInfo(5 * time.Second); serr != nil {
		res.AddStep("verify", false, serr.Error())
	} else {
		res.SelfAfter = &self
		res.AddStep("verify", true, fmt.Sprintf("now %.3f MHz bw=%.1f kHz sf=%d cr=%d tx=%d dBm",
			float64(self.FreqKHz)/1000.0, float64(self.BandwidthHz)/1000.0, self.SF, self.CR, self.TxPower))
	}

	res.OK = true
	return finish()
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

// readUptime returns the companion's current uptime in seconds, or 0 if the
// firmware does not support CMD_GET_STATS or the query fails. Best-effort.
func readUptime(link *liveLink) uint32 {
	if link == nil || link.client == nil {
		return 0
	}
	cs, err := link.client.GetCoreStats(2 * time.Second)
	if err != nil {
		return 0
	}
	return cs.UptimeSecs
}

// logRebootEvidence reads the companion uptime after a reconnect and logs
// definitive proof of whether the MCU rebooted (uptime ran backwards) or the
// USB CDC link merely hiccupped (uptime kept climbing). A zero baseline means
// we never got a reading (older firmware) and cannot prove it either way.
func logRebootEvidence(link *liveLink, before uint32, ctx string) {
	if before == 0 {
		log.Printf("%s: uptime baseline unavailable (firmware may predate CMD_GET_STATS) — cannot prove reboot vs USB hiccup", ctx)
		return
	}
	after := readUptime(link)
	if after == 0 {
		log.Printf("%s: uptime unreadable after reconnect — cannot confirm reboot", ctx)
		return
	}
	if companion.UptimeIndicatesReboot(before, after) {
		log.Printf("%s: DEVICE REBOOT CONFIRMED — companion uptime %ds → %ds (ran backwards). The MCU actually reset during the secure request; this is a real reboot, not a USB CDC glitch.", ctx, before, after)
		return
	}
	log.Printf("%s: NO reboot — companion uptime %ds → %ds (kept climbing). The USB CDC link dropped but the MCU stayed up; look at the host USB stack, not firmware TX.", ctx, before, after)
}

func disconnectHint(err error) string {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	// Precise error class (bare EOF/CDC hangup vs real syscall errno) — the key
	// signal when the host shows no USB disconnect in dmesg.
	class := companion.ClassifyErr(err)
	switch {
	case strings.Contains(msg, "no RESP_CODE_SENT"):
		// Reset happened before RESP_CODE_SENT → before the transmit was queued.
		return "companion reset BEFORE RESP_CODE_SENT (" + msg + ")" + class + " — the link dropped while the firmware was building the login packet (ECDH/crypto), before the transmit was queued. Run the advert self-test (mode=advert): its post-TX liveness check tells you whether a bare RF transmit also resets this device (→ RF/power/firmware TX) or only secure requests do (→ login/crypto path)."
	case strings.Contains(msg, "after RESP_CODE_SENT"):
		// RESP_CODE_SENT arrived → packet built OK; drop is during/after TX.
		return "companion dropped AFTER RESP_CODE_SENT, in the transmit+reply window (" + msg + ")" + class + " — RESP_CODE_SENT arrived, so packet build succeeded; the link died while the packet was actually on the air. Run the advert self-test: if its post-TX liveness check also fails, ANY RF transmit resets the device; if it passes, the fault is specific to the login/secure-request path."
	default:
		return "companion USB disconnected during login (" + msg + ")" + class + " — only one process may own the tty. Run the advert self-test (post-TX liveness check) to tell whether ANY RF transmit resets the device or only the login/secure-request path — don't assume from a command that returned before the transmit ran."
	}
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

func envBool(k string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
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
