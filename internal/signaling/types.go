package signaling

// Offer is one peer's ICE offer: local ufrag/pwd plus gathered candidates
// and a per-attempt nonce. We don't use full SDP — the structured form is
// enough for pion/ice.
//
// Gen is a random per-Connect-attempt nonce. peer-a sets it on the offer;
// peer-b echoes the received offer's Gen into its answer. peer-a polls
// AwaitAnswer and rejects answers whose Gen doesn't match what it just
// posted — this keeps a stale answer left over from a previous attempt
// (or posted by a peer-b that read a stale offer during a symmetric
// restart race) from being consumed as if it were fresh.
type Offer struct {
	Ufrag      string   `json:"ufrag"`
	Pwd        string   `json:"pwd"`
	Candidates []string `json:"candidates"`
	Gen        string   `json:"gen,omitempty"`
}

// Answer mirrors Offer.
type Answer = Offer
