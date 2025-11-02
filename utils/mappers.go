package utils

import (
	"strconv"

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
