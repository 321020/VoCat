package httpsmode

import (
	"bufio"
	"errors"
	"net"
	"sync"
	"time"
)

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (conn *bufferedConn) Read(buffer []byte) (int, error) { return conn.reader.Read(buffer) }

type channelListener struct {
	address net.Addr
	conns   chan net.Conn
	done    chan struct{}
}

func (listener *channelListener) Accept() (net.Conn, error) {
	select {
	case conn := <-listener.conns:
		if conn == nil {
			return nil, net.ErrClosed
		}
		return conn, nil
	case <-listener.done:
		return nil, net.ErrClosed
	}
}
func (listener *channelListener) Close() error   { return nil }
func (listener *channelListener) Addr() net.Addr { return listener.address }

type Multiplexer struct {
	base      net.Listener
	manager   *Manager
	plain     *channelListener
	tls       *channelListener
	done      chan struct{}
	closeOnce sync.Once
}

func NewMultiplexer(base net.Listener, manager *Manager) *Multiplexer {
	done := make(chan struct{})
	mux := &Multiplexer{
		base: base, manager: manager, done: done,
		plain: &channelListener{address: base.Addr(), conns: make(chan net.Conn, 64), done: done},
		tls:   &channelListener{address: base.Addr(), conns: make(chan net.Conn, 64), done: done},
	}
	go mux.accept()
	return mux
}

func (mux *Multiplexer) Plain() net.Listener { return mux.plain }
func (mux *Multiplexer) TLS() net.Listener   { return mux.tls }

func (mux *Multiplexer) Close() error {
	var err error
	mux.closeOnce.Do(func() {
		close(mux.done)
		err = mux.base.Close()
	})
	return err
}

func (mux *Multiplexer) accept() {
	for {
		conn, err := mux.base.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				_ = mux.Close()
			}
			return
		}
		go mux.classify(conn)
	}
}

func (mux *Multiplexer) classify(conn net.Conn) {
	reader := bufio.NewReaderSize(conn, 4096)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	first, err := reader.Peek(1)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		return
	}
	wrapped := &bufferedConn{Conn: conn, reader: reader}
	listener := mux.plain
	if first[0] == 0x16 {
		if !mux.manager.Enabled() {
			_ = conn.Close()
			return
		}
		listener = mux.tls
	}
	select {
	case listener.conns <- wrapped:
	case <-mux.done:
		_ = conn.Close()
	}
}
