package main

import (
	"errors"
	"log"
	"net/http"
	"time"
)

func main() {
	cfg, err := loadConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	app, err := newServerApp(cfg)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("AG-UI server listening on %s with model %s", cfg.Addr, cfg.ModelName)
	err = server.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
