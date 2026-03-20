package main

import (
	"fmt"
	"github.com/shanejwalsh/starhane-fm-server/cmd/api"
)

func main() {
    port := "8000"


    fmt.Println("Starting server on port:", port)

    server := api.NewAPIServer(port)
    err := server.Start()
    if err != nil {
        fmt.Println("Failed to start server:", err)
        return
    }
}
