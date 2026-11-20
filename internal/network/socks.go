package network

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
)

func (r *Route) connectSOCKS(ctx context.Context, conn net.Conn, network, address string) (net.Conn, error) {
	method := byte(0)
	if r.config.Username != "" {
		method = 2
	}
	if err := writeAll(conn, []byte{5, 1, method}); err != nil {
		return nil, fmt.Errorf("send SOCKS5 greeting: %w", err)
	}
	var response [2]byte
	if _, err := io.ReadFull(conn, response[:]); err != nil {
		return nil, fmt.Errorf("read SOCKS5 greeting: %w", err)
	}
	if response[0] != 5 || response[1] != method {
		return nil, errors.New("SOCKS5 proxy rejected the authentication method; check proxy credentials")
	}
	if method == 2 {
		packet := append([]byte{1, byte(len(r.config.Username))}, r.config.Username...)
		packet = append(packet, byte(len(r.config.Password)))
		packet = append(packet, r.config.Password...)
		if err := writeAll(conn, packet); err != nil {
			return nil, fmt.Errorf("send SOCKS5 authentication: %w", err)
		}
		if _, err := io.ReadFull(conn, response[:]); err != nil {
			return nil, fmt.Errorf("read SOCKS5 authentication: %w", err)
		}
		if response[0] != 1 || response[1] != 0 {
			return nil, errors.New("SOCKS5 authentication failed; check the proxy username and password")
		}
	}
	command, target := byte(1), address
	if network == "udp" {
		command, target = 3, "0.0.0.0:0"
	}
	encoded, err := encodeAddress(target)
	if err != nil {
		return nil, err
	}
	if err := writeAll(conn, append([]byte{5, command, 0}, encoded...)); err != nil {
		return nil, fmt.Errorf("send SOCKS5 connection request: %w", err)
	}
	var head [3]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return nil, fmt.Errorf("read SOCKS5 connection response: %w", err)
	}
	if head[0] != 5 || head[2] != 0 {
		return nil, errors.New("invalid SOCKS5 connection response")
	}
	if head[1] != 0 {
		return nil, fmt.Errorf("SOCKS5 proxy refused the connection (code %d); check proxy routing and protocol support", head[1])
	}
	bound, err := readAddress(conn)
	if err != nil {
		return nil, fmt.Errorf("read SOCKS5 bound address: %w", err)
	}
	if network != "udp" {
		return conn, nil
	}
	host, port, err := net.SplitHostPort(bound)
	if err != nil || port == "0" {
		return nil, errors.New("SOCKS5 proxy supplied an invalid UDP relay")
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host, _, _ = net.SplitHostPort(conn.RemoteAddr().String())
	}
	udp, err := (&net.Dialer{}).DialContext(ctx, "udp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("connect SOCKS5 UDP relay: %w", err)
	}
	encoded, err = encodeAddress(address)
	if err != nil {
		udp.Close()
		return nil, err
	}
	c := &socksUDPConn{Conn: udp, control: conn, target: encoded, buffer: make([]byte, 65535)}
	// RFC 1928 ties the UDP association to this TCP connection. Closing either
	// side must release both, including when the proxy terminates the session.
	go func() {
		_, _ = io.Copy(io.Discard, conn)
		c.Close()
	}()
	return c, nil
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func encodeAddress(address string) ([]byte, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid proxy destination address")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 || host == "" {
		return nil, errors.New("invalid proxy destination port or host")
	}
	var packet []byte
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			packet = append([]byte{1}, ip4...)
		} else {
			packet = append([]byte{4}, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, errors.New("SOCKS5 destination hostname is too long")
		}
		packet = append([]byte{3, byte(len(host))}, host...)
	}
	return binary.BigEndian.AppendUint16(packet, uint16(port)), nil
}

func readAddress(r io.Reader) (string, error) {
	var kind [1]byte
	if _, err := io.ReadFull(r, kind[:]); err != nil {
		return "", err
	}
	length := 0
	switch kind[0] {
	case 1:
		length = 4
	case 4:
		length = 16
	case 3:
		if _, err := io.ReadFull(r, kind[:]); err != nil {
			return "", err
		}
		length = int(kind[0])
		if length == 0 {
			return "", errors.New("empty SOCKS5 hostname")
		}
		kind[0] = 3
	default:
		return "", errors.New("invalid SOCKS5 address type")
	}
	packet := make([]byte, length+2)
	if _, err := io.ReadFull(r, packet); err != nil {
		return "", err
	}
	host := string(packet[:length])
	if kind[0] != 3 {
		host = net.IP(packet[:length]).String()
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(packet[length:])))), nil
}

type socksUDPConn struct {
	net.Conn
	control net.Conn
	target  []byte
	buffer  []byte
	readMu  sync.Mutex
	close   sync.Once
}

func (c *socksUDPConn) Close() error {
	var err error
	c.close.Do(func() {
		err = c.Conn.Close()
		c.control.Close()
	})
	return err
}

func (c *socksUDPConn) Write(p []byte) (int, error) {
	packet := append([]byte{0, 0, 0}, c.target...)
	packet = append(packet, p...)
	if len(packet) > 65507 {
		return 0, errors.New("SOCKS5 UDP packet is too large")
	}
	n, err := c.Conn.Write(packet)
	if err != nil {
		return 0, err
	}
	if n != len(packet) {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}

func (c *socksUDPConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	n, err := c.Conn.Read(c.buffer)
	if err != nil {
		return 0, err
	}
	if n < 4 || c.buffer[0] != 0 || c.buffer[1] != 0 || c.buffer[2] != 0 {
		return 0, errors.New("invalid or fragmented SOCKS5 UDP packet")
	}
	// The relay's source address precedes each payload. Consume it without
	// interpreting the audio bytes as SOCKS framing.
	reader := bytes.NewReader(c.buffer[3:n])
	if _, err := readAddress(reader); err != nil {
		return 0, fmt.Errorf("invalid SOCKS5 UDP source: %w", err)
	}
	return copy(p, c.buffer[n-reader.Len():n]), nil
}
