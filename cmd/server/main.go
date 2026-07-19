package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/charliec05/transactional-reservation-service/internal/httpapi"
	"github.com/charliec05/transactional-reservation-service/internal/reservation"
)

func main() {
	address := flag.String("address", ":8080", "HTTP listen address")
	stateFile := flag.String("state-file", "data/state.json", "path to the atomically persisted state file")
	flag.Parse()

	store, err := reservation.NewStore(*stateFile, nil)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if _, err := store.ExpireDue(); err != nil {
				log.Printf("expire holds: %v", err)
			}
		}
	}()

	server := &http.Server{
		Addr:              *address,
		Handler:           httpapi.New(store),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("reservation service listening on %s", *address)
	log.Fatal(server.ListenAndServe())
}
