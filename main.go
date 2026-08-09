package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	cfg := config{
		ListenAddr:     ":" + env("PORT", "8000"),
		TVHURL:         env("TVH_URL", "http://tvheadend:9981"),
		TVHUsername:    os.Getenv("TVH_USERNAME"),
		TVHPassword:    os.Getenv("TVH_PASSWORD"),
		StatePath:      env("STATE_PATH", "/data/state.json"),
		ServerPassword: os.Getenv("SERVER_PASSWORD"),
	}

	store, err := openStore(cfg.StatePath)
	if err != nil {
		log.Fatal(err)
	}
	tvh, err := newTVHClient(cfg.TVHURL, cfg.TVHUsername, cfg.TVHPassword)
	if err != nil {
		log.Fatal(err)
	}

	server := newServer(cfg, tvh, store)
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("UHF-compatible TVHeadend proxy listening on %s", cfg.ListenAddr)
	log.Printf("TVHeadend backend: %s", strings.TrimRight(cfg.TVHURL, "/"))
	log.Fatal(httpServer.ListenAndServe())
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
