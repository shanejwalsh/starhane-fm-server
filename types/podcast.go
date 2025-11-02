package types

type Podcast struct {
	Id            string   `json:"id"`
	Title         string   `json:"title"`
	ArtworkUrl30  string   `json:"artwork_url_30"`
	ArtworkUrl100 string   `json:"artwork_url_100"`
	ArtworkUrl600 string   `json:"artwork_url_600"`
	ArtistName    string   `json:"artist_name"`
	Genres        []string `json:"genres"`
	Explicit      bool     `json:"explicit"`
}

type Episode struct {
	Id          int    `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Url         string `json:"url"`
}
