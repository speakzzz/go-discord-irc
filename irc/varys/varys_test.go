//go:build !race
// +build !race

// These tests drive real go-ircevent connections against a fake server.
// go-ircevent reads and writes its connection state without locks, which the
// race detector reports regardless of our code, so they are skipped under
// -race.

package varys

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	irc "github.com/qaisjp/go-ircevent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeIRCServer accepts one client, welcomes it, and reports every line it
// receives on lines.
type fakeIRCServer struct {
	ln    net.Listener
	conns chan net.Conn
	lines chan string
}

func newFakeIRCServer(t *testing.T) *fakeIRCServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	s := &fakeIRCServer{ln: ln, conns: make(chan net.Conn, 4), lines: make(chan string, 256)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.conns <- c
			go s.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *fakeIRCServer) serve(c net.Conn) {
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		s.lines <- line
		if strings.HasPrefix(line, "USER ") {
			_, _ = c.Write([]byte(":fake.server 001 bob :Welcome\r\n"))
		}
	}
}

func (s *fakeIRCServer) waitFor(t *testing.T, prefix string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case l := <-s.lines:
			if strings.HasPrefix(l, prefix) {
				return l
			}
		case <-deadline:
			t.Fatalf("server never received a line starting with %q", prefix)
			return ""
		}
	}
}

func connectBob(t *testing.T, s *fakeIRCServer) (*Varys, *irc.Connection) {
	t.Helper()
	v := NewVarys()
	require.NoError(t, v.Setup(SetupParams{Server: s.ln.Addr().String()}, nil))

	welcomed := make(chan struct{}, 1)
	require.NoError(t, v.Connect(ConnectParams{
		UID: "1", Nick: "bob", Username: "bob", RealName: "bob",
		Callbacks: map[string]func(*irc.Event){
			"001": func(*irc.Event) { welcomed <- struct{}{} },
		},
	}, nil))

	select {
	case <-welcomed:
	case <-time.After(5 * time.Second):
		t.Fatal("never welcomed")
	}
	conn, ok := v.conn("1")
	require.True(t, ok)
	return v, conn
}

func TestQuitWhileConnectedSendsQuit(t *testing.T) {
	s := newFakeIRCServer(t)
	v, _ := connectBob(t, s)

	require.NoError(t, v.QuitIfConnected(QuitParams{UID: "1", QuitMessage: "bye"}, nil))
	assert.Equal(t, "QUIT :bye", s.waitFor(t, "QUIT"))

	_, ok := v.conn("1")
	assert.False(t, ok)
}

// Before: sending to a puppet whose connection had dropped filled the dead
// write buffer and then blocked the caller (the bridge loop) forever.
func TestSendToDisconnectedDoesNotBlock(t *testing.T) {
	s := newFakeIRCServer(t)
	v, conn := connectBob(t, s)

	// Take the server away so the reconnect attempt fails, then drop the
	// client's connection.
	s.ln.Close()
	(<-s.conns).Close()

	require.Eventually(t, func() bool { return !conn.Connected() }, 5*time.Second, 10*time.Millisecond)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			_ = v.SendRaw(SendRawParams{UID: "1", Messages: []string{"PRIVMSG #c :hi"}}, nil)
		}
		_ = v.Nick(NickParams{UID: "1", Nick: "bob2"}, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sending to a disconnected puppet blocked")
	}

	// Quitting a disconnected puppet must not block either; it marks the
	// connection as quitting so the reconnect loop gives up.
	quitDone := make(chan struct{})
	go func() {
		_ = v.QuitIfConnected(QuitParams{UID: "1", QuitMessage: "bye"}, nil)
		close(quitDone)
	}()
	select {
	case <-quitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("quitting a disconnected puppet blocked")
	}
	_, ok := v.conn("1")
	assert.False(t, ok)
}
