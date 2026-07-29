package main

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"
)

// discoveryRequest is what the app sends as a UDP broadcast to find a server on the local
// network. discoveryResponsePrefix is what this server replies with, followed by the port its
// HTTP API listens on - which is also the port this listener is bound to, since the app has
// nowhere else to send the broadcast to in the first place.
const (
	discoveryRequest        = "CLOUDSTREAM_SYNC_DISCOVER_V1"
	discoveryResponsePrefix = "CLOUDSTREAM_SYNC_V1 "
)

// runDiscoveryResponder answers discovery broadcasts on the same port the HTTP API listens on,
// so the app can find this server on the local network without the user typing an IP address.
//
// UDP and TCP are separate namespaces, so binding this to the same port number as the HTTP
// server is not a conflict. It is deliberately unauthenticated: a reply only reveals "a
// cloudstream-sync server is here", the same trust boundary as /healthz, nothing account or
// device specific.
func runDiscoveryResponder(ctx context.Context, addr string, log *slog.Logger) {
	port, err := portFromAddr(addr)
	if err != nil {
		log.Warn("discovery listener disabled: could not parse port", "addr", addr, "err", err)
		return
	}

	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		// Most likely a container or firewall not forwarding the UDP port. The HTTP API
		// still works via a manually entered address, so this is a warning, not fatal.
		log.Warn("discovery listener disabled", "port", port, "err", err)
		return
	}

	log.Info("discovery listener up", "port", port)
	serveDiscovery(ctx, conn, port, log)
}

// serveDiscovery runs the receive loop against an already-bound socket, separated out from
// runDiscoveryResponder so a test can supply a conn bound to an OS-assigned port (":0") rather
// than needing a fixed one that might collide with something else on the test machine.
func serveDiscovery(ctx context.Context, conn *net.UDPConn, port int, log *slog.Logger) {
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	response := []byte(discoveryResponsePrefix + strconv.Itoa(port))
	buf := make([]byte, 512)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if string(buf[:n]) != discoveryRequest {
			continue
		}
		if _, err := conn.WriteToUDP(response, from); err != nil {
			log.Warn("discovery reply failed", "to", from, "err", err)
		}
	}
}

// portFromAddr extracts the port from a listen address, which may omit the host (":9909") or
// not (e.g. "0.0.0.0:9909").
func portFromAddr(addr string) (int, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		// net.SplitHostPort rejects a bare ":9909" as missing a host in some Go versions;
		// falling back to a manual trim covers that case without depending on which.
		portStr = strings.TrimPrefix(addr, ":")
	}
	return strconv.Atoi(portStr)
}
