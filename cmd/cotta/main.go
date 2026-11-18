// Cotta — self-hosted SSH in the browser.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"cotta/internal/api"
	"cotta/internal/auth"
	"cotta/internal/session"
	"cotta/internal/store"
	"cotta/internal/vault"
	"cotta/web"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// healthcheck is what the container's HEALTHCHECK runs: the image has no
// shell or curl, so the binary probes itself.
func healthcheck(addr string) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		os.Exit(1)
	}
	c := http.Client{Timeout: 3 * time.Second}
	res, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil || res.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	os.Exit(0)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		healthcheck(env("COTTA_ADDR", ":8080"))
	}
	// Flags override the environment; handy for local development.
	addrFlag := flag.String("addr", env("COTTA_ADDR", ":8080"), "address to listen on")
	dataFlag := flag.String("data", env("COTTA_DATA", "data"), "directory for the database")
	flag.Parse()
	addr, dataDir := *addrFlag, *dataFlag
	idle, err := time.ParseDuration(env("COTTA_IDLE_LOCK", "12h"))
	if err != nil || idle < time.Minute {
		log.Fatalf("COTTA_IDLE_LOCK must be a duration of at least 1m, e.g. 12h")
	}
	cfTeam, cfAud := os.Getenv("COTTA_CF_TEAM_DOMAIN"), os.Getenv("COTTA_CF_AUD")
	if (cfTeam == "") != (cfAud == "") {
		log.Fatalf("set both COTTA_CF_TEAM_DOMAIN and COTTA_CF_AUD, or neither")
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatalf("creating data directory: %v", err)
	}
	st, err := store.Open(filepath.Join(dataDir, "cotta.db"))
	if err != nil {
		log.Fatalf("opening database: %v", err)
	}
	defer st.Close()

	v := vault.New(st)
	sessions := session.NewManager()
	a := &api.API{
		Store: st, Vault: v, Auth: auth.New(v, idle), Sessions: sessions,
		Static: web.FS, Cloudflare: cfTeam != "",
	}
	handler := a.Handler()
	if cfTeam != "" {
		handler = auth.NewCFAccess(cfTeam, cfAud).Middleware(handler)
		log.Printf("Cloudflare Access check is on: requests without a valid Access token are refused")
	} else {
		log.Printf("Cloudflare Access check is off: don't expose this port beyond a trusted network")
	}

	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("Cotta listening on %s, data in %s", addr, dataDir)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("shutting down; ending running sessions")
	sessions.CloseAll()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
