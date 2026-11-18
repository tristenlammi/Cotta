// Cotta — self-hosted SSH in the browser.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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

// firstPort is where the search for a free port starts. Inside the Docker
// image the address is fixed at :8080 and the published port is chosen by
// update.sh instead.
const firstPort = 8230

// listen opens addr. With no address configured it takes the first unused
// port counting up from firstPort.
func listen(addr string) (net.Listener, error) {
	if addr != "" {
		return net.Listen("tcp", addr)
	}
	var err error
	for port := firstPort; port < firstPort+100; port++ {
		var ln net.Listener
		if ln, err = net.Listen("tcp", fmt.Sprintf(":%d", port)); err == nil {
			if port != firstPort {
				log.Printf("port %d is in use; using %d instead", firstPort, port)
			}
			return ln, nil
		}
	}
	return nil, fmt.Errorf("no free port between %d and %d: %w", firstPort, firstPort+99, err)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		healthcheck(env("COTTA_ADDR", ":8080"))
	}
	// Flags override the environment; handy for local development.
	addrFlag := flag.String("addr", os.Getenv("COTTA_ADDR"), "address to listen on (default: first free port from 8230 up)")
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

	ln, err := listen(addr)
	if err != nil {
		log.Fatalf("server: %v", err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("Cotta listening on %s, data in %s", ln.Addr(), dataDir)
		if _, port, err := net.SplitHostPort(ln.Addr().String()); err == nil {
			log.Printf("Open http://localhost:%s", port)
		}
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
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
