package companion

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatusStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := OpenStatusStore(dir)
	info := CompanionInfo{Port: "/dev/ttyACM1", Baud: 115200, OK: true, LastOpen: time.Now().UTC()}
	snap := PollSnapshot{
		PublicKey: "aa",
		Name:      "R1",
		PolledAt:  time.Now().UTC(),
		OK:        true,
		IsAdmin:   true,
		Stats:     &RepeaterStats{BatteryMv: 3700, UptimeSecs: 10},
	}
	if err := store.Upsert(info, snap); err != nil {
		t.Fatal(err)
	}
	doc, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Companion.OK || doc.Repeaters["aa"].Stats.BatteryMv != 3700 {
		t.Fatalf("doc=%+v", doc)
	}
	fi, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("status file mode too open: %v", fi.Mode())
	}
	// Ensure valid JSON
	raw, _ := os.ReadFile(store.Path())
	var check map[string]json.RawMessage
	if err := json.Unmarshal(raw, &check); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(store.Path()) != statusFileName {
		t.Fatalf("path=%s", store.Path())
	}
}

func TestStatusStoreSetContacts(t *testing.T) {
	dir := t.TempDir()
	store := OpenStatusStore(dir)
	info := CompanionInfo{Port: "/dev/ttyACM1", Baud: 115200, OK: true}
	contacts := []Contact{
		{PublicKey: "aa", Name: "R1", Type: AdvTypeRepeater, TypeLabel: "repeater", OutPathLen: 255},
	}
	if err := store.SetContacts(info, contacts); err != nil {
		t.Fatal(err)
	}
	doc, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if doc.ContactCount != 1 || len(doc.Contacts) != 1 || doc.Contacts[0].Name != "R1" {
		t.Fatalf("doc=%+v", doc)
	}
	if doc.Companion.ContactCount != 1 {
		t.Fatalf("companion count=%d", doc.Companion.ContactCount)
	}
	if doc.ContactsAt.IsZero() {
		t.Fatal("contactsAt unset")
	}
}
