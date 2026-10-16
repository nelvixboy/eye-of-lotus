package docker

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDaemon answers the two endpoints the transport test needs.
func fakeDaemon(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	mux := http.NewServeMux()
	mux.HandleFunc("/_ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.43")
		w.Write([]byte("OK"))
	})
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	})
	go http.Serve(ln, mux)
	return sock
}

func TestCheckAgainstALiveDaemon(t *testing.T) {
	c := New(fakeDaemon(t))
	if err := c.Check(context.Background()); err != nil {
		t.Errorf("Check against a daemon that answers: %v", err)
	}
	if got := c.Socket(); !strings.HasSuffix(got, "docker.sock") {
		t.Errorf("Socket() = %q", got)
	}
}

func TestContainersAgainstAnEmptyDaemon(t *testing.T) {
	c := New(fakeDaemon(t))
	list, err := c.Containers(context.Background())
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("an empty daemon reported %d containers", len(list))
	}
}

func TestCheckAgainstAMissingSocket(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "nothing-here.sock"))
	err := c.Check(context.Background())
	if err == nil {
		t.Fatal("Check against a missing socket reported success")
	}
	// The message has to name the socket: "cannot reach the daemon" without
	// saying where it looked is the kind of error that costs an hour.
	if !strings.Contains(err.Error(), "nothing-here.sock") {
		t.Errorf("the error does not name the socket: %v", err)
	}
}

func TestDefaultSocketPath(t *testing.T) {
	c := New("")
	if c.Socket() != DefaultSocket {
		t.Errorf("Socket() = %q, want %q", c.Socket(), DefaultSocket)
	}
}

func TestContextCancellationIsReported(t *testing.T) {
	c := New(fakeDaemon(t))
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if err := c.Check(ctx); err == nil {
		t.Error("Check with an expired context reported success")
	}
}
