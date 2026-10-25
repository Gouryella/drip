package mux

import (
	"bufio"
	"net"
)

// BufferedConn wraps a connection with a buffered reader.
type BufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

// NewBufferedConn creates a new buffered connection.
func NewBufferedConn(conn net.Conn, reader *bufio.Reader) *BufferedConn {
	return &BufferedConn{Conn: conn, reader: reader}
}

// Read reads from the buffered reader if available, otherwise from the connection.
func (bc *BufferedConn) Read(p []byte) (int, error) {
	if bc.reader != nil {
		return bc.reader.Read(p)
	}
	return bc.Conn.Read(p)
}

// SessionBuilder helps build multiplexer sessions with consistent configuration.
type SessionBuilder struct {
	conn     net.Conn
	reader   *bufio.Reader
	config   *Config
	isServer bool
}

// NewSessionBuilder creates a new session builder.
func NewSessionBuilder(conn net.Conn) *SessionBuilder {
	return &SessionBuilder{conn: conn}
}

// WithReader sets the buffered reader for the session.
func (sb *SessionBuilder) WithReader(reader *bufio.Reader) *SessionBuilder {
	sb.reader = reader
	return sb
}

// WithConfig sets the multiplexer configuration.
func (sb *SessionBuilder) WithConfig(config *Config) *SessionBuilder {
	sb.config = config
	return sb
}

// AsServer configures the session as a server.
func (sb *SessionBuilder) AsServer() *SessionBuilder {
	sb.isServer = true
	if sb.config == nil {
		sb.config = NewServerConfig()
	}
	return sb
}

// AsClient configures the session as a client.
func (sb *SessionBuilder) AsClient() *SessionBuilder {
	sb.isServer = false
	if sb.config == nil {
		sb.config = NewClientConfig()
	}
	return sb
}

// Build creates the session.
func (sb *SessionBuilder) Build() (*Session, error) {
	conn := sb.conn
	if sb.reader != nil {
		conn = NewBufferedConn(sb.conn, sb.reader)
	}
	if sb.config == nil {
		if sb.isServer {
			sb.config = NewServerConfig()
		} else {
			sb.config = NewClientConfig()
		}
	}
	if sb.isServer {
		return Server(conn, sb.config)
	}
	return Client(conn, sb.config)
}

// BuildClientSession creates a client session with the given connection and reader.
func BuildClientSession(conn net.Conn, reader *bufio.Reader) (*Session, error) {
	return NewSessionBuilder(conn).WithReader(reader).AsClient().Build()
}

// BuildServerSession creates a server session with the given connection and reader.
func BuildServerSession(conn net.Conn, reader *bufio.Reader) (*Session, error) {
	return NewSessionBuilder(conn).WithReader(reader).AsServer().Build()
}
