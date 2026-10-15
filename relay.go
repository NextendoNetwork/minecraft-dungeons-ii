package main

import (
	"bytes"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
)

// A host behind a home router can't receive the joiners' first packets, so members on another
// network are sent to this relay instead. The host patch pings each host-facing port from its
// game port (7777) every couple of seconds; that keeps its router open toward the relay, and the
// relay forwards between the joiners and the host's mapped address. Host-facing port k carries
// joiner slot k (a party has at most 3 joiners); each session gets its own joiner-facing port.

const (
	relaySlots      = 3
	relayHostBase   = 37771
	relayJoinBase   = 37780
	relayJoinPorts  = 60
	relayPingMaxAge = 30 * time.Second
	// relayWaitForHost is how long a member's handoff waits for the host's first pings.
	relayWaitForHost = 30 * time.Second
	relayJoinerIdle  = 60 * time.Second
	relaySessionTTL  = 10 * time.Minute
)

var relayPing = []byte("NXRP")

type relayHost struct {
	addr  [relaySlots]*net.UDPAddr // the host's mapped address as seen on each host-facing port
	seen  time.Time
	pings int // since the last report
}

type relayJoiner struct {
	addr *net.UDPAddr
	last time.Time
}

type relaySession struct {
	id, hostIP string
	port       int
	conn       *net.UDPConn
	joiners    [relaySlots]*relayJoiner
	last       time.Time
	// packets since the last report: joiner->host forwarded, joiner packets dropped because the
	// host had no mapping for the slot, host->joiner forwarded
	toHost, noHost, toJoiner int
}

type udpRelay struct {
	mu       sync.Mutex
	publicIP string
	hostConn [relaySlots]*net.UDPConn
	joinConn [relayJoinPorts]*net.UDPConn
	hosts    map[string]*relayHost // by host public IP
	sessions map[int]*relaySession // by joiner-facing port
}

var relay *udpRelay

// startRelay opens the relay's ports; with DUNGEONS_RELAY=0 members are sent to the host directly.
func startRelay() {
	if os.Getenv("DUNGEONS_RELAY") == "0" {
		log.Printf("relay disabled")
		return
	}
	r := &udpRelay{publicIP: os.Getenv("DUNGEONS_RELAY_IP"), hosts: map[string]*relayHost{}, sessions: map[int]*relaySession{}}
	if r.publicIP == "" {
		r.publicIP = "127.0.0.1"
	}
	for k := range relaySlots {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: relayHostBase + k})
		if err != nil {
			log.Printf("relay disabled: %v", err)
			return
		}
		r.hostConn[k] = c
	}
	for i := range relayJoinPorts {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: relayJoinBase + i})
		if err != nil {
			log.Printf("relay disabled: %v", err)
			return
		}
		r.joinConn[i] = c
	}
	for k := range relaySlots {
		go r.readHost(k)
	}
	for i := range relayJoinPorts {
		go r.readJoiner(i)
	}
	go r.expire()
	relay = r
	log.Printf("relay on %s: host ports %d-%d, joiner ports %d-%d", r.publicIP, relayHostBase, relayHostBase+relaySlots-1, relayJoinBase, relayJoinBase+relayJoinPorts-1)
}

// hostReachable reports whether the host at ip has pinged every host-facing port recently.
func (r *udpRelay) hostReachable(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.hosts[ip]
	if h == nil || time.Since(h.seen) > relayPingMaxAge {
		return false
	}
	for _, a := range h.addr {
		if a == nil {
			return false
		}
	}
	return true
}

// open returns the joiner-facing port of session id, allocating one; a host has one session at a
// time, so an older session of the same host is closed.
func (r *udpRelay) open(id, hostIP string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	free := -1
	for i := range relayJoinPorts {
		s := r.sessions[relayJoinBase+i]
		switch {
		case s == nil:
			if free < 0 {
				free = i
			}
		case s.id == id:
			s.last = time.Now()
			return s.port, true
		case s.hostIP == hostIP:
			log.Printf("relay: session %s replaced by %s on port %d", s.id, id, s.port)
			delete(r.sessions, s.port)
			if free < 0 {
				free = i
			}
		}
	}
	if free < 0 {
		return 0, false
	}
	port := relayJoinBase + free
	r.sessions[port] = &relaySession{id: id, hostIP: hostIP, port: port, conn: r.joinConn[free], last: time.Now()}
	log.Printf("relay: session %s (host %s) on port %d", id, hostIP, port)
	return port, true
}

func (r *udpRelay) readHost(k int) {
	conn := r.hostConn[k]
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		ip := from.IP.String()
		if n == len(relayPing) && bytes.Equal(buf[:n], relayPing) {
			r.mu.Lock()
			h := r.hosts[ip]
			if h == nil {
				h = &relayHost{}
				r.hosts[ip] = h
				log.Printf("relay: host %s pinging", ip)
			}
			if h.addr[k] == nil || !h.addr[k].IP.Equal(from.IP) || h.addr[k].Port != from.Port {
				h.addr[k] = from
			}
			h.seen = time.Now()
			h.pings++
			r.mu.Unlock()
			continue
		}
		r.mu.Lock()
		var to *net.UDPAddr
		var out *net.UDPConn
		for _, s := range r.sessions {
			if s.hostIP == ip && s.joiners[k] != nil {
				to, out = s.joiners[k].addr, s.conn
				s.last = time.Now()
				s.toJoiner++
				break
			}
		}
		r.mu.Unlock()
		if to != nil {
			out.WriteToUDP(buf[:n], to)
		}
	}
}

func (r *udpRelay) readJoiner(i int) {
	conn := r.joinConn[i]
	port := relayJoinBase + i
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		r.mu.Lock()
		s := r.sessions[port]
		slot := -1
		if s != nil {
			now := time.Now()
			for k, j := range s.joiners {
				if j != nil && j.addr.IP.Equal(from.IP) && j.addr.Port == from.Port {
					slot = k
					break
				}
			}
			if slot < 0 {
				for k, j := range s.joiners {
					if j == nil || now.Sub(j.last) > relayJoinerIdle {
						slot = k
						s.joiners[k] = &relayJoiner{addr: from}
						log.Printf("relay: session %s joiner %s in slot %d", s.id, from, k)
						break
					}
				}
			}
			if slot >= 0 {
				s.joiners[slot].last = now
				s.last = now
			}
		}
		var to *net.UDPAddr
		if slot >= 0 {
			if h := r.hosts[s.hostIP]; h != nil {
				to = h.addr[slot]
			}
			if to != nil {
				s.toHost++
			} else {
				s.noHost++
			}
		}
		r.mu.Unlock()
		if to != nil {
			r.hostConn[slot].WriteToUDP(buf[:n], to)
		}
	}
}

// expire reports and resets the traffic counters every 30 s, and drops idle sessions and hosts.
func (r *udpRelay) expire() {
	for range time.Tick(30 * time.Second) {
		r.mu.Lock()
		for ip, h := range r.hosts {
			if h.pings > 0 {
				log.Printf("relay: host %s sent %d pings in 30s", ip, h.pings)
				h.pings = 0
			}
		}
		for port, s := range r.sessions {
			if s.toHost+s.noHost+s.toJoiner > 0 {
				log.Printf("relay: session %s port %d: joiner->host %d, dropped (no host mapping) %d, host->joiner %d",
					s.id, port, s.toHost, s.noHost, s.toJoiner)
				s.toHost, s.noHost, s.toJoiner = 0, 0, 0
			}
		}
		for port, s := range r.sessions {
			if time.Since(s.last) > relaySessionTTL {
				log.Printf("relay: session %s on port %d expired", s.id, port)
				delete(r.sessions, port)
			}
		}
		for ip, h := range r.hosts {
			if time.Since(h.seen) > relaySessionTTL {
				delete(r.hosts, ip)
			}
		}
		r.mu.Unlock()
	}
}

func relayAddress(r *udpRelay, port int) string {
	return r.publicIP + ":" + strconv.Itoa(port)
}
