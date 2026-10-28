package bigolistener

import (
	"bytes"
	"context"
	"crypto/rc4"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestLoginUsesWebTicketAndVerifiedNativeIdentity(t *testing.T) {
	for _, username := range []string{"viewer-account", "0"} {
		t.Run(username, func(t *testing.T) { testWebTicketLogin(t, username) })
	}
}

func testWebTicketLogin(t *testing.T, username string) {
	t.Helper()
	t.Parallel()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	newCipher := func() *rc4.Cipher {
		c, err := rc4.NewCipher([]byte("cookie-derived-ticket-test"))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	session := validTestSession()
	session.Username = username
	client := &linkdClient{conn: clientConn, sendCipher: newCipher(), recvCipher: newCipher(), session: session}
	server := &linkdClient{conn: serverConn, sendCipher: newCipher(), recvCipher: newCipher()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		done <- func() error {
			frame, err := server.readFrame(ctx)
			if err != nil {
				return err
			}
			if frame.URI != uriPrepareLogin {
				return fmt.Errorf("prepare login URI: %d", frame.URI)
			}
			r := &wireReader{data: frame.Body}
			uid, _ := r.i32()
			cookie, _ := r.blob()
			secret, _ := r.string()
			username, _ := r.string()
			deviceID, _ := r.string()
			if uint32(uid) != session.UID || !bytes.Equal(cookie, session.Cookie) || secret != "0" || username != session.Username || deviceID != session.DeviceID {
				return fmt.Errorf("incorrect web credential or secret in native login")
			}
			identity, err := r.take(25)
			wantIdentity := make([]byte, 25)
			wantIdentity[22] = 1 // native client type; remaining identity defaults are zero
			if err != nil || !bytes.Equal(identity, wantIdentity) {
				return fmt.Errorf("incorrect native identity fields")
			}
			ip, _ := r.i32()
			proxy, _ := r.u16()
			stamp, _ := r.i32()
			mcc, _ := r.string()
			mnc, _ := r.string()
			country, _ := r.string()
			security, _ := r.blob()
			uuid, _ := r.i32()
			extra, err := r.i32()
			if err != nil || ip != 0 || proxy != 0 || stamp != 0 || mcc != "" || mnc != "" || country != "CN" || len(security) != 0 || uuid != 0 || extra != 0 || r.remaining() != 0 {
				return fmt.Errorf("Android identity leaked into web-ticket login")
			}
			response := &wireWriter{}
			response.u32(session.UID)
			response.i32(protocolStatusOK)
			if err := server.sendFrame(uriPrepareLoginRes, response.Bytes()); err != nil {
				return err
			}
			frame, err = server.readFrame(ctx)
			if err != nil {
				return err
			}
			r = &wireReader{data: frame.Body}
			country, err = r.string()
			if frame.URI != uriLogin || err != nil || country != "CN" {
				return fmt.Errorf("incorrect second login stage")
			}
			response = &wireWriter{}
			response.i32(0)
			return server.sendFrame(uriLoginRes, response.Bytes())
		}()
	}()
	if err := client.login(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
