package pgtest

import (
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Proxy forwards TCP connections to the test database and can be taken down
// and brought back on the same address, imitating a database outage.
type Proxy struct {
	t      testing.TB
	target string
	addr   string

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]bool
}

// NewProxy starts a proxy to the test database and returns it with a DSN
// that connects through it.
func NewProxy(t testing.TB) (*Proxy, string) {
	t.Helper()
	dsn := DSN(t)
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Host == "" {
		t.Skipf("%s must be a postgres:// URL to proxy it", EnvDSN)
	}
	target := parsed.Host
	if !strings.Contains(target, ":") {
		target += ":5432"
	}
	p := &Proxy{t: t, target: target, conns: map[net.Conn]bool{}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.addr = listener.Addr().String()
	p.serve(listener)
	t.Cleanup(p.Down)
	parsed.Host = p.addr
	return p, parsed.String()
}

func (p *Proxy) serve(listener net.Listener) {
	p.mu.Lock()
	p.listener = listener
	p.mu.Unlock()
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", p.target)
			if err != nil {
				client.Close()
				continue
			}
			p.mu.Lock()
			p.conns[client], p.conns[server] = true, true
			p.mu.Unlock()
			go p.pipe(client, server)
			go p.pipe(server, client)
		}
	}()
}

func (p *Proxy) pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	dst.Close()
	src.Close()
	p.mu.Lock()
	delete(p.conns, dst)
	delete(p.conns, src)
	p.mu.Unlock()
}

// Down stops accepting connections and drops the open ones.
func (p *Proxy) Down() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listener != nil {
		p.listener.Close()
		p.listener = nil
	}
	for conn := range p.conns {
		conn.Close()
	}
	p.conns = map[net.Conn]bool{}
}

// Up accepts connections again on the same address.
func (p *Proxy) Up() {
	p.t.Helper()
	p.mu.Lock()
	running := p.listener != nil
	p.mu.Unlock()
	if running {
		return
	}
	listener, err := net.Listen("tcp", p.addr)
	if err != nil {
		p.t.Fatal(err)
	}
	p.serve(listener)
}
