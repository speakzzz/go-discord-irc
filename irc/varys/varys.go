// Package varys is an abstraction that allows you to add or remove puppets,
// and receive a snapshot of state via an RPC-based interface.
//
// Why "varys"? Because it is the Master of Whisperers.
package varys

import (
	"crypto/tls"
	"fmt"
	"strings"
	"sync"

	irc "github.com/qaisjp/go-ircevent"
)

type Varys struct {
	connConfig SetupParams

	// mu guards uidToConns, which is used from the bridge loop and from
	// IRC callback goroutines.
	mu         sync.RWMutex
	uidToConns map[string]*irc.Connection
}

func (v *Varys) conn(uid string) (*irc.Connection, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	conn, ok := v.uidToConns[uid]
	return conn, ok
}

func NewVarys() *Varys {
	return &Varys{uidToConns: make(map[string]*irc.Connection)}
}

// connCall runs fn on one connection, or all of them if uid is blank.
// fn is called without holding the lock.
func (v *Varys) connCall(uid string, fn func(*irc.Connection)) {
	if uid == "" {
		v.mu.RLock()
		conns := make([]*irc.Connection, 0, len(v.uidToConns))
		for _, conn := range v.uidToConns {
			conns = append(conns, conn)
		}
		v.mu.RUnlock()

		for _, conn := range conns {
			fn(conn)
		}
		return
	}

	if conn, ok := v.conn(uid); ok {
		fn(conn)
	}
}

type Client interface {
	Setup(params SetupParams) error
	GetUIDToNicks() (map[string]string, error)
	Connect(params ConnectParams) error // Does not yet support netClient
	QuitIfConnected(uid string, quitMsg string) error
	Nick(uid string, nick string) error

	// SendRaw supports a blank uid to send to all connections.
	SendRaw(uid string, params InterpolationParams, messages ...string) error
	// GetNick gets the current connection's nick
	GetNick(uid string) (string, error)
	// Connected returns the status of the current connection
	Connected(uid string) (bool, error)
}

type SetupParams struct {
	UseTLS             bool // Whether we should use TLS
	InsecureSkipVerify bool // Controls tls.Config.InsecureSkipVerify, if using TLS

	Server         string
	ServerPassword string
	WebIRCPassword string
}

func (v *Varys) Setup(params SetupParams, _ *struct{}) error {
	v.connConfig = params
	return nil
}

func (v *Varys) GetUIDToNicks(_ struct{}, result *map[string]string) error {
	v.mu.RLock()
	m := make(map[string]string, len(v.uidToConns))
	for uid, conn := range v.uidToConns {
		m[uid] = conn.GetNick()
	}
	v.mu.RUnlock()
	*result = m
	return nil
}

type ConnectParams struct {
	UID string

	Nick     string
	Username string
	RealName string

	WebIRCSuffix string

	// TODO(qaisjp): does not support net/rpc!!!!
	Callbacks map[string]func(*irc.Event)
}

func (v *Varys) Connect(params ConnectParams, _ *struct{}) error {
	conn := irc.IRC(params.Nick, params.Username)
	// conn.Debug = true
	conn.RealName = params.RealName

	// TLS things, and the server password
	conn.Password = v.connConfig.ServerPassword
	conn.UseTLS = v.connConfig.UseTLS
	if v.connConfig.InsecureSkipVerify {
		conn.TLSConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	// Set up WebIRC, if a suffix is provided
	if params.WebIRCSuffix != "" {
		conn.WebIRC = v.connConfig.WebIRCPassword + " " + params.WebIRCSuffix
	}

	// On kick, rejoin the channel
	conn.AddCallback("KICK", func(e *irc.Event) {
		if e.Arguments[1] == conn.GetNick() {
			conn.Join(e.Arguments[0])
		}
	})

	for eventcode, callback := range params.Callbacks {
		conn.AddCallback(eventcode, callback)
	}

	// Register before connecting: callbacks (e.g. 001) can fire as soon as
	// Connect starts its read loop, and they look the connection up by UID.
	v.mu.Lock()
	v.uidToConns[params.UID] = conn
	v.mu.Unlock()

	err := conn.Connect(v.connConfig.Server)
	if err != nil {
		v.mu.Lock()
		if v.uidToConns[params.UID] == conn {
			delete(v.uidToConns, params.UID)
		}
		v.mu.Unlock()
		return fmt.Errorf("error opening irc connection: %w", err)
	}

	go conn.Loop()
	return nil
}

type QuitParams struct {
	UID         string
	QuitMessage string
}

func (v *Varys) QuitIfConnected(params QuitParams, _ *struct{}) error {
	v.mu.Lock()
	conn, ok := v.uidToConns[params.UID]
	delete(v.uidToConns, params.UID)
	v.mu.Unlock()

	if ok && conn.Connected() {
		conn.QuitMessage = params.QuitMessage
		conn.Quit()
	}
	return nil
}

type InterpolationParams struct {
	Nick bool
}
type SendRawParams struct {
	UID      string
	Messages []string

	Interpolation InterpolationParams
}

func (v *Varys) SendRaw(params SendRawParams, _ *struct{}) error {
	v.connCall(params.UID, func(c *irc.Connection) {
		for _, msg := range params.Messages {
			if params.Interpolation.Nick {
				msg = strings.ReplaceAll(msg, "${NICK}", c.GetNick())
			}
			c.SendRaw(msg)
		}
	})
	return nil
}

func (v *Varys) GetNick(uid string, result *string) error {
	if conn, ok := v.conn(uid); ok {
		*result = conn.GetNick()
	}
	return nil
}

func (v *Varys) Connected(uid string, result *bool) error {
	if conn, ok := v.conn(uid); ok {
		*result = conn.Connected()
	}

	return nil
}

type NickParams struct {
	UID  string
	Nick string
}

func (v *Varys) Nick(params NickParams, _ *struct{}) error {
	if conn, ok := v.conn(params.UID); ok {
		conn.Nick(params.Nick)
	}
	return nil
}
