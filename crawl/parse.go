package crawl

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/shanejwalsh/itunes-xml-parser/feeds"

	"github.com/shanejwalsh/starhane-fm-server/store"
)

// ParsedFeed is a feed document reduced to what we store.
type ParsedFeed struct {
	Title string
	// NewFeedURL is <itunes:new-feed-url>, the publisher saying the feed has
	// permanently moved. Empty when absent.
	NewFeedURL string
	Episodes   []store.EpisodeUpsert
}

// ParseFeed turns a feed document into storable episodes.
func ParseFeed(body []byte) (ParsedFeed, error) {
	rss, err := feeds.ParseBytes(body)
	if err != nil {
		return ParsedFeed{}, fmt.Errorf("parsing feed: %w", err)
	}

	channel := rss.Channel
	parsed := ParsedFeed{
		Title:      strings.TrimSpace(channel.Title),
		NewFeedURL: strings.TrimSpace(channel.NewFeedURL),
		Episodes:   make([]store.EpisodeUpsert, 0, len(channel.Item)),
	}

	duplicated := duplicatedGuids(channel.Item)

	for i, item := range channel.Item {
		guid, source := episodeIdentity(item, i, duplicated)
		parsed.Episodes = append(parsed.Episodes, store.EpisodeUpsert{
			Guid:        guid,
			GuidSource:  source,
			Title:       strings.TrimSpace(item.Title),
			Description: item.Description,
			AudioURL:    strings.TrimSpace(item.Enclosure.URL),
			AudioLength: parseLength(item.Enclosure.Length),
			AudioType:   strings.TrimSpace(item.Enclosure.Type),
			Author:      strings.TrimSpace(item.Author),
			PubDate:     ParsePubDate(item.PubDate),
			PubDateRaw:  item.PubDate,
			Link:        strings.TrimSpace(item.Link),
			// The API has always treated exactly "true" as explicit; keeping
			// that rule keeps the response unchanged.
			Explicit:    item.Explicit == "true",
			Duration:    strings.TrimSpace(item.Duration),
			EpisodeNo:   parseNumber(item.EpisodeNumber),
			SeasonNo:    parseNumber(item.SeasonNumber),
			EpisodeType: strings.TrimSpace(item.EpisodeType),
			ImageURL:    strings.TrimSpace(item.Image.Href),
			Position:    int32(i),
		})
	}

	return parsed, nil
}

// duplicatedGuids reports which guids appear more than once in a document.
//
// A repeated guid cannot identify an episode, and which copy "wins" would
// otherwise depend on item order. Counting first means every copy falls back,
// so identities stay stable across crawls even if the publisher reorders items.
func duplicatedGuids(items []feeds.Episode) map[string]bool {
	counts := make(map[string]int, len(items))
	for _, item := range items {
		if guid := strings.TrimSpace(item.Guid.Text); guid != "" {
			counts[guid]++
		}
	}

	duplicated := make(map[string]bool)
	for guid, count := range counts {
		if count > 1 {
			duplicated[guid] = true
		}
	}
	return duplicated
}

// episodeIdentity picks a stable key for an episode within its feed.
//
// The feed's <guid> is used when it is present and unique. Otherwise we hash
// the enclosure URL, which is the next most stable thing an episode has. Only
// when there is no enclosure either do we fall back to the item's content,
// which is the best available but will change if the publisher edits the title.
func episodeIdentity(item feeds.Episode, position int, duplicated map[string]bool) (guid, source string) {
	if g := strings.TrimSpace(item.Guid.Text); g != "" && !duplicated[g] {
		return g, store.GuidFromFeed
	}

	if enclosure := strings.TrimSpace(item.Enclosure.URL); enclosure != "" {
		return hash(enclosure), store.GuidFromEnclosureHash
	}

	return hash(fmt.Sprintf("%d\x00%s\x00%s", position, item.Title, item.PubDate)), store.GuidDerived
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// pubDateLayouts covers RFC 822/1123 with and without zones and day names, plus
// RFC 3339, which between them account for what podcast feeds actually emit.
var pubDateLayouts = []string{
	time.RFC1123Z,
	time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"Mon, 02 Jan 2006 15:04:05 -0700",
	"Mon, 02 Jan 2006 15:04 -0700",
	"Mon, 02 Jan 2006 15:04:05",
	"2 Jan 2006 15:04:05 -0700",
	"02 Jan 2006 15:04:05 -0700",
	"02 Jan 2006 15:04:05 MST",
	time.RFC822Z,
	time.RFC822,
	time.RFC3339,
	"2006-01-02T15:04:05.000Z",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// ParsePubDate parses a feed's publication date, returning nil when it cannot.
//
// A date we cannot read is not an error: the raw string is stored and served
// regardless, and the parsed value only feeds scheduling.
func ParsePubDate(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	for _, layout := range pubDateLayouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			utc := parsed.UTC()
			return &utc
		}
	}
	return nil
}

// parseLength reads an enclosure's byte count, returning 0 when it is missing
// or nonsense — which is what the API has always reported for such feeds.
func parseLength(raw string) int64 {
	length, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || length < 0 {
		return 0
	}
	return length
}

// parseNumber reads an itunes:episode or itunes:season value. Feeds put
// non-numeric junk in these, so failure means "not stated" rather than an error.
func parseNumber(raw string) *int32 {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 32)
	if err != nil {
		return nil
	}
	v := int32(n)
	return &v
}
