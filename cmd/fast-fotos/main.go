package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/andrewl/fast-fotos/internal/app"
)

// main initializes configuration, creates the application server, and starts the HTTP listener.
func main() {
	config, err := app.ConfigFromEnvironment()
	if err != nil {
		log.Fatal(err)
	}

	server, err := app.NewServer(context.Background(), config)
	if err != nil {
		log.Fatal(err)
	}
	defer server.Close()

	httpServer := &http.Server{
		Addr:              config.ListenAddress,
		Handler:           server.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("fast-fotos listening on http://%s", config.ListenAddress)
	log.Fatal(httpServer.ListenAndServe())
}
