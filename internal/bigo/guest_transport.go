package bigo

import (
	"context"
	"crypto/rc4"
	"encoding/binary"
	"errors"
	"net"
	"strconv"
	"time"
)

// Guest audio uses datagrams. Keep each datagram intact while the native packet
// reader consumes its header and body through separate Read calls.
type nativeDatagramConn struct {
	net.Conn
	buffer  [65536]byte
	pending []byte
}

func (c *nativeDatagramConn) Read(out []byte) (int, error) {
	if len(out) == 0 {
		return 0, nil
	}
	for len(c.pending) == 0 {
		n, err := c.Conn.Read(c.buffer[:])
		if err != nil {
			return 0, err
		}
		packet := c.buffer[:n]
		if validNativeDatagram(packet) {
			c.pending = packet
		} else if len(packet) > 4 {
			cipher, err := rc4.NewCipher(packet[:4])
			if err != nil {
				return 0, err
			}
			cipher.XORKeyStream(packet[4:], packet[4:])
			if validNativeDatagram(packet[4:]) {
				c.pending = packet[4:]
			}
		}
	}
	n := copy(out, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func validNativeDatagram(packet []byte) bool {
	if len(packet) < 4 {
		return false
	}
	word := binary.LittleEndian.Uint32(packet)
	if word&0x80000000 != 0 {
		return int(word>>4&0xfff) == len(packet)
	}
	return len(packet) >= 10 && int(word) == len(packet)
}

func dialGuestAudio(ctx context.Context, routes []mediaProxy, viewer, sid uint32, attempt int) (*nativeMediaConn, error) {
	var addresses []string
	for _, route := range routes {
		var ip [4]byte
		binary.LittleEndian.PutUint32(ip[:], route.ip)
		for _, port := range route.udp {
			if port != 0 {
				addresses = append(addresses, net.JoinHostPort(net.IP(ip[:]).String(), strconv.Itoa(int(port))))
			}
		}
	}
	if len(addresses) == 0 {
		return nil, errors.New("BIGO supplied no UDP route for guest audio")
	}
	address := addresses[attempt%len(addresses)]
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "udp", address)
	if err != nil {
		return nil, err
	}
	return &nativeMediaConn{Conn: &nativeDatagramConn{Conn: conn}, viewer: viewer, sid: sid, guest: true}, nil
}

func guestAudioLoginBody(channel *pJoinChannelRes) []byte {
	b := binary.LittleEndian.AppendUint32(nil, channel.source)
	b = binary.LittleEndian.AppendUint32(b, channel.sid)
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = appendNativeBlob(b, channel.cookie)
	b = binary.LittleEndian.AppendUint32(b, channel.timestamp)
	b = binary.LittleEndian.AppendUint32(b, 0x60db7)
	b = append(b, 1)
	b = binary.LittleEndian.AppendUint64(b, uint64(time.Now().UnixMilli()))
	b = binary.LittleEndian.AppendUint32(b, 1)
	b = appendNativeBytes(b, nil)
	b = appendNativeBytes(b, []byte{0})
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = append(b, 0)
	b = appendNativeBlob(b, nil)
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = appendNativeBlob(b, nil)
	b = appendNativeBlob(b, nil)
	for _, value := range []uint32{10, 0, 0} {
		b = binary.LittleEndian.AppendUint32(b, value)
	}
	b = binary.LittleEndian.AppendUint32(b, 2)
	for _, value := range []string{"client_version", nativeMediaClientVersion, "tos", "0"} {
		b = appendNativeBlob(b, []byte(value))
	}
	b = binary.LittleEndian.AppendUint32(b, 0)
	return append(b, 4)
}

func (c *nativeMediaConn) subscribeGuestAudio() error {
	c.subscription = uint64(time.Now().UnixMilli())
	b := binary.LittleEndian.AppendUint32(nil, c.viewer)
	b = binary.LittleEndian.AppendUint32(b, 2)
	b = binary.LittleEndian.AppendUint64(b, c.subscription)
	b = appendNativeBytes(b, nil)
	b = appendNativeBytes(b, nil)
	b = appendNativeBytes(b, []byte{2})
	b = binary.LittleEndian.AppendUint32(b, c.sid)
	b = binary.LittleEndian.AppendUint64(b, uint64(c.viewer))
	b = binary.LittleEndian.AppendUint64(b, uint64(c.sid))
	// Extended identifiers, with server mixing disabled.
	b = append(b, 4)
	return c.send(0x5902, b)
}

func marshalGuestPing(channel *pJoinChannelRes) []byte {
	b := binary.LittleEndian.AppendUint32(nil, channel.source)
	b = binary.LittleEndian.AppendUint32(b, channel.sid)
	b = binary.LittleEndian.AppendUint32(b, uint32(time.Now().UnixMilli()))
	b = binary.LittleEndian.AppendUint32(b, channel.timestamp)
	b = binary.LittleEndian.AppendUint32(b, 0)
	return appendNativeBlob(b, nil)
}
