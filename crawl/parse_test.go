package crawl

import (
	"fmt"
	"testing"
	"time"

	"github.com/shanejwalsh/starhane-fm-server/store"
)

// feedWith wraps items in a minimal but valid RSS document.
func feedWith(channelExtra, items string) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">
  <channel>
    <title>Test Podcast</title>
    %s
    %s
  </channel>
</rss>`, channelExtra, items))
}

func TestParseFeedUsesGuidWhenPresent(t *testing.T) {
	parsed, err := ParseFeed(feedWith("", `
		<item>
		  <title>One</title>
		  <guid>stable-guid</guid>
		  <enclosure url="https://example.com/1.mp3" length="999" type="audio/mpeg"/>
		</item>`))
	if err != nil {
		t.Fatal(err)
	}

	if len(parsed.Episodes) != 1 {
		t.Fatalf("got %d episodes, want 1", len(parsed.Episodes))
	}
	e := parsed.Episodes[0]
	if e.Guid != "stable-guid" {
		t.Errorf("guid = %q, want stable-guid", e.Guid)
	}
	if e.GuidSource != store.GuidFromFeed {
		t.Errorf("guid_source = %q, want %q", e.GuidSource, store.GuidFromFeed)
	}
	if e.AudioLength != 999 {
		t.Errorf("audio length = %d, want 999", e.AudioLength)
	}
	if e.Position != 0 {
		t.Errorf("position = %d, want 0", e.Position)
	}
}

func TestParseFeedFallsBackToEnclosureHashWhenGuidMissing(t *testing.T) {
	parsed, err := ParseFeed(feedWith("", `
		<item>
		  <title>No guid</title>
		  <enclosure url="https://example.com/audio.mp3" length="1" type="audio/mpeg"/>
		</item>`))
	if err != nil {
		t.Fatal(err)
	}

	e := parsed.Episodes[0]
	if e.GuidSource != store.GuidFromEnclosureHash {
		t.Fatalf("guid_source = %q, want %q", e.GuidSource, store.GuidFromEnclosureHash)
	}
	if e.Guid != hash("https://example.com/audio.mp3") {
		t.Errorf("guid = %q, want the hash of the enclosure URL", e.Guid)
	}

	// The same enclosure must hash the same way on every crawl, otherwise
	// every crawl would insert duplicate episodes.
	again, err := ParseFeed(feedWith("", `
		<item>
		  <title>Retitled, same audio</title>
		  <enclosure url="https://example.com/audio.mp3" length="1" type="audio/mpeg"/>
		</item>`))
	if err != nil {
		t.Fatal(err)
	}
	if again.Episodes[0].Guid != e.Guid {
		t.Error("the same enclosure URL produced two different guids")
	}
}

func TestParseFeedFallsBackWhenGuidIsDuplicated(t *testing.T) {
	// Both copies must fall back, not just the second: otherwise which item
	// keeps the shared guid would depend on the order the publisher happens to
	// emit them in.
	parsed, err := ParseFeed(feedWith("", `
		<item>
		  <title>First</title>
		  <guid>same</guid>
		  <enclosure url="https://example.com/a.mp3" length="1" type="audio/mpeg"/>
		</item>
		<item>
		  <title>Second</title>
		  <guid>same</guid>
		  <enclosure url="https://example.com/b.mp3" length="1" type="audio/mpeg"/>
		</item>`))
	if err != nil {
		t.Fatal(err)
	}

	if len(parsed.Episodes) != 2 {
		t.Fatalf("got %d episodes, want 2", len(parsed.Episodes))
	}
	for i, e := range parsed.Episodes {
		if e.GuidSource != store.GuidFromEnclosureHash {
			t.Errorf("episode %d guid_source = %q, want %q", i, e.GuidSource, store.GuidFromEnclosureHash)
		}
		if e.Guid == "same" {
			t.Errorf("episode %d kept the duplicated guid", i)
		}
	}
	if parsed.Episodes[0].Guid == parsed.Episodes[1].Guid {
		t.Error("the two episodes ended up with the same guid")
	}
}

func TestParseFeedDerivesGuidWithNoGuidAndNoEnclosure(t *testing.T) {
	parsed, err := ParseFeed(feedWith("", `
		<item><title>Nothing to key on</title><pubDate>Wed, 01 Jan 2025 00:00:00 +0000</pubDate></item>`))
	if err != nil {
		t.Fatal(err)
	}

	e := parsed.Episodes[0]
	if e.GuidSource != store.GuidDerived {
		t.Errorf("guid_source = %q, want %q", e.GuidSource, store.GuidDerived)
	}
	if e.Guid == "" {
		t.Error("an episode must always get some key")
	}
}

func TestParseFeedReadsNewFeedURL(t *testing.T) {
	parsed, err := ParseFeed(feedWith(`<itunes:new-feed-url>https://example.com/moved.xml</itunes:new-feed-url>`, ""))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.NewFeedURL != "https://example.com/moved.xml" {
		t.Errorf("new feed url = %q, want https://example.com/moved.xml", parsed.NewFeedURL)
	}
}

func TestParseFeedPreservesRawPubDate(t *testing.T) {
	const raw = "Wed, 01 Jan 2025 09:30:00 +0000"
	parsed, err := ParseFeed(feedWith("", `<item><guid>g</guid><pubDate>`+raw+`</pubDate></item>`))
	if err != nil {
		t.Fatal(err)
	}

	e := parsed.Episodes[0]
	// The API has always served the feed's own date string verbatim.
	if e.PubDateRaw != raw {
		t.Errorf("pub_date_raw = %q, want %q", e.PubDateRaw, raw)
	}
	if e.PubDate == nil {
		t.Fatal("pub_date should have parsed")
	}
	if got := e.PubDate.Format(time.RFC3339); got != "2025-01-01T09:30:00Z" {
		t.Errorf("pub_date = %s, want 2025-01-01T09:30:00Z", got)
	}
}

func TestParseFeedRejectsMalformedXML(t *testing.T) {
	if _, err := ParseFeed([]byte("<html><body>Not a feed")); err == nil {
		t.Error("expected an error for a non-feed document")
	}
}

func TestParsePubDate(t *testing.T) {
	cases := map[string]string{
		"Wed, 01 Jan 2025 00:00:00 +0000": "2025-01-01T00:00:00Z",
		"Wed, 1 Jan 2025 00:00:00 +0000":  "2025-01-01T00:00:00Z",
		"Wed, 01 Jan 2025 00:00:00 GMT":   "2025-01-01T00:00:00Z",
		"2025-01-01T00:00:00Z":            "2025-01-01T00:00:00Z",
		"Wed, 01 Jan 2025 02:00:00 +0200": "2025-01-01T00:00:00Z",
		"01 Jan 2025 00:00:00 +0000":      "2025-01-01T00:00:00Z",
	}
	for raw, want := range cases {
		got := ParsePubDate(raw)
		if got == nil {
			t.Errorf("ParsePubDate(%q) = nil, want %s", raw, want)
			continue
		}
		if formatted := got.Format(time.RFC3339); formatted != want {
			t.Errorf("ParsePubDate(%q) = %s, want %s", raw, formatted, want)
		}
	}

	// An unreadable date is not an error: the raw string is still stored and
	// served, only scheduling loses the signal.
	for _, raw := range []string{"", "   ", "not a date", "yesterday"} {
		if got := ParsePubDate(raw); got != nil {
			t.Errorf("ParsePubDate(%q) = %v, want nil", raw, got)
		}
	}
}

func TestParseLengthAndNumber(t *testing.T) {
	lengths := map[string]int64{"123": 123, "": 0, "abc": 0, "-5": 0, " 42 ": 42}
	for raw, want := range lengths {
		if got := parseLength(raw); got != want {
			t.Errorf("parseLength(%q) = %d, want %d", raw, got, want)
		}
	}

	if got := parseNumber("7"); got == nil || *got != 7 {
		t.Errorf("parseNumber(%q) = %v, want 7", "7", got)
	}
	for _, raw := range []string{"", "bonus", "1.5"} {
		if got := parseNumber(raw); got != nil {
			t.Errorf("parseNumber(%q) = %v, want nil", raw, got)
		}
	}
}
