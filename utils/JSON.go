package utils

import (
	"encoding/json"
	"fmt"
	"net/http"
)

func ParseJSON(req *http.Request, payload any) error {
	if req.Body == nil {
		return fmt.Errorf("missing request body")
	}
	return json.NewDecoder(req.Body).Decode(payload)
}

func WriteJson(res http.ResponseWriter, status int, v any) error {
	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(status)

	return json.NewEncoder(res).Encode(v)
}

func WriteError(res http.ResponseWriter, status int, err error) {

	WriteJson(res, status, map[string]string{"error": err.Error()})
}
