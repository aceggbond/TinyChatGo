package server

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

var proxyProtocolV2Signature = []byte("\r\n\r\n\x00\r\nQUIT\n")

// proxyProtocolListener accepts HAProxy PROXY protocol v1/v2 only from peers
// explicitly configured as trusted proxies. Connections without a PROXY
// prefix continue unchanged, so the same listener supports direct clients and
// FRP listeners configured with transport.proxyProtocolVersion.
type proxyProtocolListener struct {
	net.Listener
	trusted []*net.IPNet
}

type proxyProtocolConn struct {
	net.Conn
	reader *bufio.Reader
	remote net.Addr
}

func (c *proxyProtocolConn) Read(buffer []byte) (int, error) {
	return c.reader.Read(buffer)
}

func (c *proxyProtocolConn) RemoteAddr() net.Addr {
	return c.remote
}

func newProxyProtocolListener(listener net.Listener, trusted []*net.IPNet) net.Listener {
	if listener == nil || len(trusted) == 0 {
		return listener
	}
	return &proxyProtocolListener{Listener: listener, trusted: append([]*net.IPNet(nil), trusted...)}
}

func (l *proxyProtocolListener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	peerIP := addressIP(connection.RemoteAddr())
	if !proxyPeerTrusted(peerIP, l.trusted) {
		return connection, nil
	}
	wrapped := &proxyProtocolConn{
		Conn: connection, reader: bufio.NewReaderSize(connection, 4096), remote: connection.RemoteAddr(),
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	matched, parseErr := wrapped.readProxyHeader()
	_ = connection.SetReadDeadline(time.Time{})
	if parseErr != nil {
		_ = connection.Close()
		return nil, parseErr
	}
	if !matched {
		return wrapped, nil
	}
	return wrapped, nil
}

func (c *proxyProtocolConn) readProxyHeader() (bool, error) {
	prefix, err := c.reader.Peek(6)
	if err != nil {
		// A slow or short direct connection is not a malformed PROXY request;
		// leave any peeked bytes buffered for net/http or TLS.
		return false, nil
	}
	if bytes.Equal(prefix, []byte("PROXY ")) {
		return true, c.readProxyV1()
	}
	if !bytes.Equal(prefix, proxyProtocolV2Signature[:6]) {
		return false, nil
	}
	header, err := c.reader.Peek(16)
	if err != nil || !bytes.Equal(header[:12], proxyProtocolV2Signature) {
		return true, errors.New("invalid PROXY protocol v2 header")
	}
	return true, c.readProxyV2(header)
}

func (c *proxyProtocolConn) readProxyV1() error {
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read PROXY protocol v1 header: %w", err)
	}
	if len(line) > 108 || !strings.HasSuffix(line, "\r\n") {
		return errors.New("invalid PROXY protocol v1 header")
	}
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 2 && fields[1] == "UNKNOWN" {
		return nil
	}
	if len(fields) != 6 || fields[0] != "PROXY" || fields[1] != "TCP4" && fields[1] != "TCP6" {
		return errors.New("invalid PROXY protocol v1 address")
	}
	ip := net.ParseIP(fields[2])
	port, err := strconv.Atoi(fields[4])
	if ip == nil || err != nil || port < 1 || port > 65535 {
		return errors.New("invalid PROXY protocol v1 source")
	}
	c.remote = &net.TCPAddr{IP: ip, Port: port}
	return nil
}

func (c *proxyProtocolConn) readProxyV2(header []byte) error {
	if len(header) < 16 || header[12]>>4 != 2 {
		return errors.New("invalid PROXY protocol v2 version")
	}
	command := header[12] & 0x0f
	family := header[13]
	payloadLength := int(binary.BigEndian.Uint16(header[14:16]))
	if payloadLength > 4096 {
		return errors.New("PROXY protocol v2 header is too large")
	}
	if _, err := c.reader.Discard(16); err != nil {
		return err
	}
	payload := make([]byte, payloadLength)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return fmt.Errorf("read PROXY protocol v2 payload: %w", err)
	}
	if command == 0 {
		return nil
	}
	if command != 1 {
		return errors.New("invalid PROXY protocol v2 command")
	}
	switch family {
	case 0x11: // TCP over IPv4
		if len(payload) < 12 {
			return errors.New("short PROXY protocol IPv4 address")
		}
		c.remote = &net.TCPAddr{IP: net.IP(append([]byte(nil), payload[:4]...)), Port: int(binary.BigEndian.Uint16(payload[8:10]))}
	case 0x21: // TCP over IPv6
		if len(payload) < 36 {
			return errors.New("short PROXY protocol IPv6 address")
		}
		c.remote = &net.TCPAddr{IP: net.IP(append([]byte(nil), payload[:16]...)), Port: int(binary.BigEndian.Uint16(payload[32:34]))}
	default:
		// UNSPEC/unsupported families are valid but cannot improve RemoteAddr.
	}
	return nil
}

func addressIP(address net.Addr) net.IP {
	if tcp, ok := address.(*net.TCPAddr); ok {
		return tcp.IP
	}
	host, _, err := net.SplitHostPort(address.String())
	if err != nil {
		return nil
	}
	return net.ParseIP(strings.Trim(host, "[]"))
}

func proxyPeerTrusted(ip net.IP, trusted []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, network := range trusted {
		if network != nil && network.Contains(ip) {
			return true
		}
	}
	return false
}
