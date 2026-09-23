package utils

import (
	"strconv"

	"github.com/shanejwalsh/itunes-xml-parser/itunes"

	"github.com/shanejwalsh/starhane-fm-server/store"
	"github.com/shanejwalsh/starhane-fm-server/types"
)

// MapPodcast turns an iTunes result into the API's podcast shape.
func MapPodcast(podcast *itunes.Result) types.Podcast {
	return types.Podcast{
		Id:            strconv.Itoa(podcast.CollectionID),
		Title:         podcast.CollectionName,
		ArtistName:    podcast.ArtistName,
		Genres:        podcast.Genres,
		ArtworkUrl600: podcast.ArtworkURL600,
		ArtworkUrl100: podcast.ArtworkURL100,
		ArtworkUrl30:  podcast.ArtworkURL30,
		Explicit:      podcast.CollectionExplicitness != "notExplicit",
	}
}

// MapStoredPodcast turns a stored podcast into the API's podcast shape.
//
// It must produce the same JSON as MapPodcast, since a request served from the
// catalogue should be indistinguishable from one served from iTunes.
func MapStoredPodcast(podcast *store.Podcast) types.Podcast {
	id := ""
	if podcast.ItunesID != nil {
		id = strconv.FormatInt(*podcast.ItunesID, 10)
	}

	return types.Podcast{
		Id:            id,
		Title:         podcast.Title,
		ArtistName:    podcast.ArtistName,
		Genres:        podcast.Genres,
		ArtworkUrl600: podcast.ArtworkURL600,
		ArtworkUrl100: podcast.ArtworkURL100,
		ArtworkUrl30:  podcast.ArtworkURL30,
		Explicit:      podcast.Explicit,
	}
}

// MapPodcastUpsert turns an iTunes result into a row to store. This is the only
// place FeedURL escapes the iTunes response — it is what the crawler needs and
// what types.Podcast deliberately does not carry.
func MapPodcastUpsert(podcast *itunes.Result) store.PodcastUpsert {
	return store.PodcastUpsert{
		ItunesID:      int64(podcast.CollectionID),
		Title:         podcast.CollectionName,
		ArtistName:    podcast.ArtistName,
		ArtworkURL30:  podcast.ArtworkURL30,
		ArtworkURL100: podcast.ArtworkURL100,
		ArtworkURL600: podcast.ArtworkURL600,
		Genres:        podcast.Genres,
		Explicit:      podcast.CollectionExplicitness != "notExplicit",
		FeedURL:       podcast.FeedURL,
	}
}

// MapStoredEpisode turns a stored episode into the API's episode shape.
//
// PubDate is the feed's original date string rather than the parsed timestamp,
// because that is what this endpoint has always returned.
func MapStoredEpisode(episode *store.Episode) types.EpisodeResponse {
	return types.EpisodeResponse{
		ID:          episode.Guid,
		Title:       episode.Title,
		Description: episode.Description,
		AudioURL:    episode.AudioURL,
		AudioLength: int(episode.AudioLength),
		Author:      episode.Author,
		PubDate:     episode.PubDateRaw,
		Link:        episode.Link,
		IsExplicit:  episode.Explicit,
		Duration:    episode.Duration,
	}
}
