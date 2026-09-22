package feedbuilder

// Serve the generated feed over HTTP for testing / small deployments.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

const (
	serveHeaderTimeout   = 10 * time.Second
	serveShutdownTimeout = 5 * time.Second
)

func serve(ctx context.Context, directory, host string, port int) error {
	if _, err := os.Stat(directory); err != nil {
		return fmt.Errorf("%w does not exist yet: %s (run 'build' first)", errOutputDir, directory)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(directory)))

	addr := net.JoinHostPort(host, strconv.Itoa(port))

	shownHost := host
	if host == "0.0.0.0" || host == "::" {
		shownHost = "<this-host>"
	}

	fmt.Printf("Serving %s at http://%s:%d/  (Ctrl-C to stop)\n", directory, shownHost, port)

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: serveHeaderTimeout}

	go func() {
		<-ctx.Done()

		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), serveShutdownTimeout)
		defer cancel()

		_ = srv.Shutdown(shutdown)
	}()

	err := srv.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve %s: %w", addr, err)
	}

	return nil
}
