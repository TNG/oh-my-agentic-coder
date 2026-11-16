// Command echomodel runs the echo model server until interrupted and prints
// its base URL on stdout.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/TNG/oh-my-agentic-coder/internal/e2e/echomodel"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address (127.0.0.1 only)")
	dumpDir := flag.String("dump-dir", "", "write every request as JSON into this directory")
	flag.Parse()

	s := echomodel.New(echomodel.Options{Addr: *addr, DumpDir: *dumpDir})
	if err := s.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer s.Close()
	fmt.Println(s.URL())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}
