package main

// Tiny mDNS browser for Android wireless-debugging services:
//   _adb-tls-pairing._tcp.local  – phone is showing "Pair with QR / code"
//   _adb-tls-connect._tcp.local  – phone has wireless debugging ON (port changes!)

import (
	"encoding/binary"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	svcPairing = "_adb-tls-pairing._tcp.local"
	svcConnect = "_adb-tls-connect._tcp.local"
)

type MdnsService struct {
	Instance string // e.g. "adb-XXXXSERIAL-abc123" or "scrcpy-k3j2h1"
	Service  string // svcPairing / svcConnect
	IP       string
	Port     int
}

func (s MdnsService) Addr() string { return net.JoinHostPort(s.IP, strconv.Itoa(s.Port)) }

func buildMdnsQuery(names ...string) []byte {
	msg := make([]byte, 12)
	binary.BigEndian.PutUint16(msg[4:], uint16(len(names)))
	for _, n := range names {
		for _, label := range strings.Split(strings.TrimSuffix(n, "."), ".") {
			msg = append(msg, byte(len(label)))
			msg = append(msg, label...)
		}
		msg = append(msg, 0)
		msg = append(msg, 0, 12)   // PTR
		msg = append(msg, 0x80, 1) // QU + IN
	}
	return msg
}

func readName(msg []byte, off int) (string, int, error) {
	var labels []string
	jumped := false
	end := 0
	for hops := 0; hops < 64; hops++ {
		if off >= len(msg) {
			return "", 0, errors.New("bad name")
		}
		l := int(msg[off])
		switch {
		case l == 0:
			if !jumped {
				end = off + 1
			}
			return strings.Join(labels, "."), end, nil
		case l&0xC0 == 0xC0:
			if off+1 >= len(msg) {
				return "", 0, errors.New("bad ptr")
			}
			if !jumped {
				end = off + 2
			}
			jumped = true
			off = int(binary.BigEndian.Uint16(msg[off:]) & 0x3FFF)
		default:
			if off+1+l > len(msg) {
				return "", 0, errors.New("bad label")
			}
			labels = append(labels, string(msg[off+1:off+1+l]))
			off += 1 + l
		}
	}
	return "", 0, errors.New("loop")
}

type mdnsRecords struct {
	ptr map[string][]string // service -> instance full names
	srv map[string]struct {
		target string
		port   int
	}
	a map[string]string
}

func parseMdns(msg []byte, rec *mdnsRecords) error {
	if len(msg) < 12 {
		return errors.New("short")
	}
	qd := int(binary.BigEndian.Uint16(msg[4:]))
	total := int(binary.BigEndian.Uint16(msg[6:])) + int(binary.BigEndian.Uint16(msg[8:])) + int(binary.BigEndian.Uint16(msg[10:]))
	off := 12
	for i := 0; i < qd; i++ {
		_, n, err := readName(msg, off)
		if err != nil {
			return err
		}
		off = n + 4
	}
	for i := 0; i < total; i++ {
		name, n, err := readName(msg, off)
		if err != nil {
			return err
		}
		off = n
		if off+10 > len(msg) {
			return errors.New("short rr")
		}
		typ := binary.BigEndian.Uint16(msg[off:])
		rdlen := int(binary.BigEndian.Uint16(msg[off+8:]))
		rd := off + 10
		if rd+rdlen > len(msg) {
			return errors.New("short rdata")
		}
		lname := strings.ToLower(name)
		switch typ {
		case 12: // PTR
			if t, _, err := readName(msg, rd); err == nil {
				rec.ptr[lname] = append(rec.ptr[lname], t)
			}
		case 33: // SRV
			if rdlen >= 7 {
				port := int(binary.BigEndian.Uint16(msg[rd+4:]))
				if t, _, err := readName(msg, rd+6); err == nil {
					rec.srv[lname] = struct {
						target string
						port   int
					}{strings.ToLower(t), port}
				}
			}
		case 1: // A
			if rdlen == 4 {
				rec.a[lname] = net.IP(msg[rd : rd+4]).String()
			}
		}
		off = rd + rdlen
	}
	return nil
}

type packet struct {
	data []byte
	src  string
}

// BrowseMdns sends queries on every IPv4 interface and collects answers for `wait`.
func BrowseMdns(wait time.Duration) []MdnsService {
	query := buildMdnsQuery(svcPairing, svcConnect)
	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

	var mu sync.Mutex
	var pkts []packet
	var wg sync.WaitGroup
	deadline := time.Now().Add(wait)

	listen := func(c *net.UDPConn) {
		defer wg.Done()
		buf := make([]byte, 9000)
		c.SetReadDeadline(deadline)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			d := make([]byte, n)
			copy(d, buf[:n])
			mu.Lock()
			pkts = append(pkts, packet{d, from.IP.String()})
			mu.Unlock()
		}
	}

	var conns []*net.UDPConn
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			ip4 := ipn.IP.To4()
			c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip4})
			if err != nil {
				continue
			}
			setMulticastIf(c, ip4)
			conns = append(conns, c)
			wg.Add(1)
			go listen(c)
			c.WriteToUDP(query, group)
		}
		// Also hear multicast replies on 5353 (best effort; may be busy).
		if mc, err := net.ListenMulticastUDP("udp4", &ifc, group); err == nil {
			conns = append(conns, mc)
			wg.Add(1)
			go listen(mc)
		}
	}
	// Re-send once halfway for lossy Wi-Fi.
	time.Sleep(wait / 3)
	for _, c := range conns {
		if la, ok := c.LocalAddr().(*net.UDPAddr); ok && la.Port != 5353 {
			c.WriteToUDP(query, group)
		}
	}
	wg.Wait()
	for _, c := range conns {
		c.Close()
	}
	return collectServices(pkts)
}

func collectServices(pkts []packet) []MdnsService {
	var out []MdnsService
	seen := map[string]bool{}
	for _, p := range pkts {
		rec := &mdnsRecords{ptr: map[string][]string{}, srv: map[string]struct {
			target string
			port   int
		}{}, a: map[string]string{}}
		if parseMdns(p.data, rec) != nil {
			continue
		}
		for _, svc := range []string{svcPairing, svcConnect} {
			for _, inst := range rec.ptr[svc] {
				s, ok := rec.srv[strings.ToLower(inst)]
				if !ok {
					continue
				}
				ip := rec.a[s.target]
				if ip == "" {
					ip = p.src
				}
				instName := strings.TrimSuffix(inst, "."+svc)
				instName = strings.TrimSuffix(instName, "."+strings.ToUpper(svc))
				if idx := strings.Index(strings.ToLower(inst), "."+svc); idx > 0 {
					instName = inst[:idx]
				}
				key := svc + "|" + instName + "|" + ip + "|" + strconv.Itoa(s.port)
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, MdnsService{Instance: instName, Service: svc, IP: ip, Port: s.port})
			}
		}
	}
	return out
}
