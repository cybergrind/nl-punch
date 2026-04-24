package signaling

// Offer is one peer's ICE offer: local ufrag/pwd plus gathered candidates.
// We don't use full SDP — the structured form is enough for pion/ice.
type Offer struct {
	Ufrag      string   `json:"ufrag"`
	Pwd        string   `json:"pwd"`
	Candidates []string `json:"candidates"`
}

// Answer mirrors Offer.
type Answer = Offer
