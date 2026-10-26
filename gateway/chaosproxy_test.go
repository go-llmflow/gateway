package gateway

import (
	"net"
	"sync"
	"testing"
)

// chaosProxy sits between the gateway and a dependency (PostgreSQL) and
// injects faults: refuse (the server is down: new connections are reset,
// open ones are cut), blackhole (a network partition: connections are
// accepted and kept, nothing is forwarded either way) and heal.
type chaosProxy struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	mode  string // "", "refuse", "blackhole"
	conns map[net.Conn]bool
	held  []net.Conn // accepted during a blackhole
	cond  *sync.Cond
}

func newChaosProxy(t *testing.T, target string) *chaosProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &chaosProxy{ln: ln, target: target, conns: map[net.Conn]bool{}}
	p.cond = sync.NewCond(&p.mu)
	go p.accept()
	t.Cleanup(func() { ln.Close(); p.set("refuse") })
	return p
}

func (p *chaosProxy) addr() string { return p.ln.Addr().String() }

func (p *chaosProxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		mode := p.mode
		if mode == "blackhole" {
			p.held = append(p.held, c)
		}
		p.mu.Unlock()
		switch mode {
		case "refuse":
			c.Close()
		case "blackhole":
		default:
			go p.pipe(c)
		}
	}
}

func (p *chaosProxy) pipe(c net.Conn) {
	u, err := net.Dial("tcp", p.target)
	if err != nil {
		c.Close()
		return
	}
	p.mu.Lock()
	p.conns[c], p.conns[u] = true, true
	p.mu.Unlock()
	copyGated := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				// A partition: bytes read are held until the network heals.
				p.mu.Lock()
				for p.mode == "blackhole" {
					p.cond.Wait()
				}
				dead := p.mode == "refuse"
				p.mu.Unlock()
				if dead {
					break
				}
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		c.Close()
		u.Close()
	}
	go copyGated(u, c)
	copyGated(c, u)
	p.mu.Lock()
	delete(p.conns, c)
	delete(p.conns, u)
	p.mu.Unlock()
}

// set changes the mode: "refuse" also cuts every open connection (the
// server restarted or died), "" heals (held connections are then served).
func (p *chaosProxy) set(mode string) {
	p.mu.Lock()
	p.mode = mode
	var cut []net.Conn
	if mode == "refuse" {
		for c := range p.conns {
			cut = append(cut, c)
		}
		cut = append(cut, p.held...)
		p.held = nil
	}
	var held []net.Conn
	if mode == "" {
		held, p.held = p.held, nil
	}
	p.cond.Broadcast()
	p.mu.Unlock()
	for _, c := range cut {
		c.Close()
	}
	for _, c := range held {
		go p.pipe(c)
	}
}
