package config

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// runHealthcheck asks a running panel whether it is alive and returns an
// error describing the failure when it is not. It exists so that the
// container image needs neither curl nor wget: the binary checks itself.
func runHealthcheck(cfg Config) error {
	addr := cfg.Listen
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		addr = net.JoinHostPort(host, port)
	}
	url := "http://" + addr + strings.TrimSuffix(cfg.BasePath, "/") + "/api/health"

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("healthcheck: %s is not answering: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s answered %s", url, resp.Status)
	}
	return ErrHandled
}
