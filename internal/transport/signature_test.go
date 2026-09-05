package transport

import (
	"encoding/json"
	"fiatjaf.com/nostr"
	"testing"
)

func TestCanonicalEventIDKnownNostrEvent(t *testing.T) {
	// Published event fixture from fiatjaf.com/nostr/event_test.go (Unlicense).
	const raw = `{"kind":1,"id":"dc90c95f09947507c1044e8f48bcf6350aa6bff1507dd4acfc755b9239b5c962","pubkey":"3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d","created_at":1644271588,"tags":[],"content":"now that https://blueskyweb.org/blog/2-7-2022-overview was announced we can stop working on nostr?","sig":"230e9d8f0ddaf7eb70b5f7741ccfa37e87a455c9a469282e3464e2052d3192cd63a167e196e381ef9d7e69e9ea43af2443b839974dc85d8aaab9efe1d9296524"}`
	var event nostr.Event
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatal(err)
	}
	if err := Valid(event); err != nil {
		t.Fatal(err)
	}
}
func TestCanonicalEventUnicodeAndEscaping(t *testing.T) {
	event := nostr.Event{CreatedAt: 42, Kind: 1, Tags: nostr.Tags{{"x", "tab\tnewline\nnull\x00"}}, Content: "quotes \" slash \\ amp & < > unicode ☃ \u2028 \u2029"}
	// Independent Python json.dumps(ensure_ascii=False,separators=(',',':')) hash.
	if got := EventID(event).Hex(); got != "6bab2a1a7c5589dfadef9cd9748e901156636efdc49b5e636f890573e0735303" {
		t.Fatal("noncanonical JSON", got)
	}
}
