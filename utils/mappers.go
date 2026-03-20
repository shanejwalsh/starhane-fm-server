package utils

import (
	"strconv"

	"github.com/shanejwalsh/itunes-xml-parser/feeds"
	"github.com/shanejwalsh/itunes-xml-parser/itunes"
	"github.com/shanejwalsh/starhane-fm-server/types"
)

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

func MapToEpisodeResponse(item *feeds.Episode) types.EpisodeResponse {
	audioLength := 0
	if len, err := strconv.Atoi(item.Enclosure.Length); err == nil {
		audioLength = len
	}

	isExplicit := item.Explicit == "true"

	return types.EpisodeResponse{
		ID:          item.Guid.Text,
		Title:       item.Title,
		Description: item.Description,
		AudioURL:    item.Enclosure.URL,
		AudioLength: audioLength,
		Author:      item.Author,
		PubDate:     item.PubDate,
		Link:        item.Link,
		IsExplicit:  isExplicit,
		Duration:    item.Duration,
	}
}
