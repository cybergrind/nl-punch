// Command nl-punch-signaling runs the in-memory rendezvous server that
// peers use to exchange ICE offers/answers. It keeps no persistent state
// (offers and answers live in memory and are short-lived), so restart is
// cheap. Any HTTP server exposing the same four endpoints will do; this
// binary exists as a ready-made reference.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"nl-punch/internal/signaling"
)

func main() {
	var (
		listen = flag.String("listen", ":8901", "HTTP listen address")
		secret = flag.String("secret", os.Getenv("NLPUNCH_SECRET"), "shared secret (X-Nl-Punch-Secret header); empty = no auth")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := signaling.NewServer(*secret)

	log.Info("signaling server starting", "listen", *listen, "auth", *secret != "")
	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Error("serve failed", "err", err)
		os.Exit(1)
	}
}
