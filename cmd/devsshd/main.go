// devsshd runs the fake test SSH server on localhost so the Cotta UI can be
// tried end to end without a real machine. Development only.
package main

import (
	"log"
	"os"
	"os/signal"

	"cotta/internal/testssh"
)

func main() {
	addr := "127.0.0.1:2222"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	s, err := testssh.Start(addr, "demo", "demo-password")
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fake SSH server on %s — user demo, password demo-password", s.Addr)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
}
