package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"
)

// listenOnFreePort binds to an OS-assigned UDP port so the test cannot collide with anything
// else already listening on the machine running it.
func listenOnFreePort(t *testing.T) (*net.UDPConn, int) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	port := conn.LocalAddr().(*net.UDPAddr).Port
	return conn, port
}

func TestDiscoveryRespondsToProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	conn, port := listenOnFreePort(t)
	go serveDiscovery(ctx, conn, port, log)

	client, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}
	defer client.Close()

	serverAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	if _, err := client.WriteToUDP([]byte(discoveryRequest), serverAddr); err != nil {
		t.Fatalf("send probe: %v", err)
	}

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, _, err := client.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}

	got := string(buf[:n])
	want := discoveryResponsePrefix + strconv.Itoa(port)
	if got != want {
		t.Fatalf("unexpected reply: got %q, want %q", got, want)
	}
}

// Anything that is not the exact expected probe string must be ignored, not echoed back -
// otherwise this listener becomes a trivial network amplification/reflection vector.
func TestDiscoveryIgnoresUnknownPayloads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	conn, port := listenOnFreePort(t)
	go serveDiscovery(ctx, conn, port, log)

	client, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}
	defer client.Close()

	serverAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	if _, err := client.WriteToUDP([]byte("not a real probe"), serverAddr); err != nil {
		t.Fatalf("send bogus probe: %v", err)
	}

	_ = client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 512)
	if _, _, err := client.ReadFromUDP(buf); err == nil {
		t.Fatal("expected no reply to an unrecognised payload")
	}
}

func TestPortFromAddr(t *testing.T) {
	cases := map[string]int{
		":9909":          9909,
		"0.0.0.0:9909":   9909,
		"127.0.0.1:8080": 8080,
	}
	for addr, want := range cases {
		got, err := portFromAddr(addr)
		if err != nil {
			t.Fatalf("portFromAddr(%q): %v", addr, err)
		}
		if got != want {
			t.Fatalf("portFromAddr(%q) = %d, want %d", addr, got, want)
		}
	}
}
