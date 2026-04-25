package config

import "testing"

// TestLoad_acceptsAnyProfileString — the profile knob is removed, so
// configs in the wild that still carry a transport.profile entry must
// load without error. We don't care what it says; it's ignored.
func TestLoad_acceptsAnyProfileString(t *testing.T) {
	body := `{
      "session_id": "s",
      "signaling": {"url": "http://x"},
      "transport": {"profile": "ludicrous-speed"},
      "peers": {"peer-a": {}, "peer-b": {}}
    }`
	p := writeConfig(t, body)
	if _, err := Load(p); err != nil {
		t.Fatalf("Load with arbitrary transport.profile must succeed; got: %v", err)
	}
}

// TestLoad_acceptsMissingTransportBlock — and configs that don't carry a
// transport block at all must also load.
func TestLoad_acceptsMissingTransportBlock(t *testing.T) {
	body := `{
      "session_id": "s",
      "signaling": {"url": "http://x"},
      "peers": {"peer-a": {}, "peer-b": {}}
    }`
	p := writeConfig(t, body)
	if _, err := Load(p); err != nil {
		t.Fatalf("Load with no transport block must succeed; got: %v", err)
	}
}
