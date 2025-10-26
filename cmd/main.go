package main

import (
	"fmt"

	"github.com/shanejwalsh/starhane-fm-server/cmd/api"
)

func main() {

	server := api.NewAPIServer("8000")

	err := server.Start()
	if err != nil {
		fmt.Println("Failed to start server:", err)
		return
	}
}
