type EpisodeResponse struct {
    ID          string `json:"id"`
    Title       string `json:"title"`
    Description string `json:"description"`
    AudioURL    string `json:"audioUrl"`
    AudioLength int    `json:"audioLength"`
    Author      string `json:"author"`
    PubDate     string `json:"pubDate"`
    Link        string `json:"link"`
    IsExplicit  bool   `json:"isExplicit"`
}
