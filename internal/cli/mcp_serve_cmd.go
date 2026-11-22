package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/gateway"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
	"github.com/TNG/oh-my-agentic-coder/internal/sidecarauth"
)

func runMCPServe(args []string, env *Env) int {
	fs := flag.NewFlagSet("mcp serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	defPath := fs.String("def", "", "path to the snapshot mcp.yaml")
	if err := fs.Parse(args); err != nil || *defPath == "" || fs.NArg() > 0 {
		fmt.Fprintln(env.Stderr, "usage: omac mcp serve --def <path>")
		return ExitMisuse
	}

	port, err := strconv.Atoi(os.Getenv("SIDECAR_PORT"))
	if err != nil || port < 1 || port > 65535 {
		fmt.Fprintln(env.Stderr, "omac mcp serve: SIDECAR_PORT is missing or not a port number")
		return ExitMisuse
	}
	secret := os.Getenv(sidecarauth.EnvVar)
	if secret == "" {
		fmt.Fprintf(env.Stderr, "omac mcp serve: %s is missing or empty\n", sidecarauth.EnvVar)
		return ExitMisuse
	}

	absDef, err := filepath.Abs(*defPath)
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac mcp serve: %v\n", err)
		return ExitIOError
	}
	raw, err := os.ReadFile(absDef)
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac mcp serve: read definition: %v\n", err)
		return ExitIOError
	}
	def, err := mcpreg.ParseDefinition(raw)
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac mcp serve: %v\n", err)
		return ExitConfigInvalid
	}
	if _, err := exec.LookPath(def.Server.Command[0]); err != nil {
		fmt.Fprintf(env.Stderr, "omac mcp serve: server command %q not found: %v\n", def.Server.Command[0], err)
		return ExitPrerequisiteMissing
	}

	handler, shutdown, err := gateway.Handler(gateway.Options{
		Def:      def,
		Dir:      filepath.Dir(absDef),
		Secret:   secret,
		ChildEnv: os.Environ(),
		Log:      env.Stderr,
	})
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac mcp serve: %v\n", err)
		return ExitGeneric
	}
	defer shutdown()

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac mcp serve: %v\n", err)
		return ExitIOError
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		fmt.Fprintf(env.Stderr, "omac mcp serve: %v\n", err)
		return ExitGeneric
	case <-ctx.Done():
	}
	shutdown()
	closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(closeCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		srv.Close()
	}
	return ExitOK
}
