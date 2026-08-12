// Command server menjalankan backend LiveKit Greenpark: penerbit access token,
// pengelola room/meeting lintas divisi, egress (rekam & streaming), dispatch
// voice AI agent, dan penerima webhook LiveKit.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"greenpark/livekit/internal/authmw"
	"greenpark/livekit/internal/config"
	"greenpark/livekit/internal/lk"
	"greenpark/livekit/internal/service"
	"greenpark/livekit/internal/store"
	httptransport "greenpark/livekit/internal/transport/http"
)

func main() {
	cfg := config.Load()

	st, err := store.New(cfg.DataPath)
	if err != nil {
		log.Fatalf("livekit: gagal membuka data store %q: %v", cfg.DataPath, err)
	}
	log.Printf("livekit: data store = %s", cfg.DataPath)

	lkc := lk.New(lk.Config{
		URL:       cfg.LiveKitURL,
		APIKey:    cfg.LiveKitAPIKey,
		APISecret: cfg.LiveKitAPISecret,
		TokenTTL:  cfg.TokenTTL,
	})
	if lkc.Enabled() {
		log.Printf("livekit: server = %s (token TTL %s)", cfg.LiveKitURL, cfg.TokenTTL)
	} else {
		log.Printf("livekit: PERINGATAN — LIVEKIT_API_KEY/LIVEKIT_API_SECRET kosong; endpoint token/room/egress akan menolak (503)")
	}

	svc := service.New(st, lkc, service.Options{
		RecordDir: cfg.RecordDir,
		AgentName: cfg.AgentName,
		TokenTTL:  cfg.TokenTTL,
		PublicURL: cfg.LiveKitPublicURL,
	})
	if cfg.LiveKitPublicURL != "" {
		log.Printf("livekit: alamat untuk browser = %s", cfg.LiveKitPublicURL)
	}

	verifier, err := authmw.New(authmw.Options{
		JWKSURL:    cfg.AuthJWKSURL,
		Department: cfg.Department,
		Issuer:     cfg.AuthIssuer,
	})
	if err != nil {
		log.Fatalf("livekit: auth verifier: %v", err)
	}
	log.Printf("livekit: SSO aktif (jwks=%s issuer=%s)", cfg.AuthJWKSURL, cfg.AuthIssuer)

	handler := httptransport.NewHandler(svc, verifier)
	router := httptransport.NewRouter(handler, cfg.AllowOrigin)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("livekit API listening on http://localhost:%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("livekit: server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("livekit: shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("livekit: graceful shutdown failed: %v", err)
	}
	log.Println("livekit: stopped")
}
