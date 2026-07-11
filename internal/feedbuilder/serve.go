package feedbuilder

// Serve the generated feed over HTTP for testing / small deployments.

import (
	"fmt"
	"net"
	"net/http"
	"os"
)

func serve(directory, host string, port int) error {
	if _, err := os.Stat(directory); err != nil {
		return fmt.Errorf("output directory does not exist yet: %s (run 'build' first)", directory)
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(directory)))

	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	shownHost := host
	if host == "0.0.0.0" || host == "::" {
		shownHost = "<this-host>"
	}
	fmt.Printf("Serving %s at http://%s:%d/  (Ctrl-C to stop)\n", directory, shownHost, port)
	return http.ListenAndServe(addr, mux)
}
