package main

import (
	"net"
	"testing"
	"time"
)

func TestRelayForwardsBothWays(t *testing.T) {
	t.Setenv("DUNGEONS_RELAY_IP", "127.0.0.1")
	startRelay()
	if relay == nil {
		t.Fatal("relay did not start")
	}
	host, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	for k := range relaySlots {
		host.WriteToUDP(relayPing, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: relayHostBase + k})
	}
	deadline := time.Now().Add(2 * time.Second)
	for !relay.hostReachable("127.0.0.1") {
		if time.Now().After(deadline) {
			t.Fatal("host pings not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	port, ok := relay.open("s1", "127.0.0.1")
	if !ok {
		t.Fatal("no joiner port")
	}

	joiner, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer joiner.Close()
	joiner.Write([]byte("hello host"))

	buf := make([]byte, 64)
	host.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, from, err := host.ReadFromUDP(buf)
	if err != nil || string(buf[:n]) != "hello host" {
		t.Fatalf("host got %q, %v", buf[:n], err)
	}
	if from.Port != relayHostBase {
		t.Fatalf("first joiner should use slot 0's host port, came from %d", from.Port)
	}

	host.WriteToUDP([]byte("hello joiner"), from)
	joiner.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err = joiner.Read(buf)
	if err != nil || string(buf[:n]) != "hello joiner" {
		t.Fatalf("joiner got %q, %v", buf[:n], err)
	}
}
