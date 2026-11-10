package bigolistener

import (
	"bytes"
	"context"
	"crypto/rc4"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLinkdAddressFailoverForPrepareLoginIPRestriction(t *testing.T) {
	for _, test := range []struct {
		name           string
		prepare, login int32
		secondPrepare  int32
		wantAttempts   int
		wantError      string
	}{
		{name: "IP restriction during prepare", prepare: 10, wantAttempts: 2},
		{name: "all addresses restricted", prepare: 10, secondPrepare: 10, wantAttempts: 2, wantError: "prepare-login result=10"},
		{name: "other prepare rejection", prepare: 9, wantAttempts: 1, wantError: "prepare-login result=9"},
		{name: "same code during final login", prepare: protocolStatusOK, login: 10, wantAttempts: 1, wantError: "login result=10"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			session := validTestSession()
			session.LinkdAddresses = []string{"first.example:1234", "second.example:1234"}
			var attempts []string
			client, address, err := dialLinkd(ctx, session, func(ctx context.Context, address string, _ Session) (*linkdClient, error) {
				attempts = append(attempts, address)
				if address == session.LinkdAddresses[0] {
					return nil, testLoginRejection(t, ctx, test.prepare, test.login)
				}
				if test.secondPrepare != 0 {
					return nil, testLoginRejection(t, ctx, test.secondPrepare, 0)
				}
				return &linkdClient{}, nil
			})
			if !slices.Equal(attempts, session.LinkdAddresses[:test.wantAttempts]) {
				t.Fatalf("dialed %v, want %v, error %v", attempts, session.LinkdAddresses[:test.wantAttempts], err)
			}
			if test.wantError == "" {
				if err != nil || client == nil || address != session.LinkdAddresses[1] {
					t.Fatalf("fallback connection failed: address %q, error %v", address, err)
				}
				return
			}
			if err == nil || client != nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("expected %q in the connection error, got %v", test.wantError, err)
			}
			if test.secondPrepare != 0 {
				for _, address := range attempts {
					if !strings.Contains(err.Error(), address) {
						t.Fatalf("missing failed address %q in %v", address, err)
					}
				}
			}
		})
	}
}

func testLoginRejection(t *testing.T, ctx context.Context, prepare, login int32) error {
	t.Helper()
	_, client, server := queryTestConnection(t)
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		done <- func() error {
			if _, err := server.awaitURI(ctx, uriPrepareLogin); err != nil {
				return err
			}
			body := &wireWriter{}
			body.u32(client.session.UID)
			body.i32(prepare)
			if err := server.sendFrame(uriPrepareLoginRes, body.Bytes()); err != nil || prepare != protocolStatusOK {
				return err
			}
			if _, err := server.awaitURI(ctx, uriLogin); err != nil {
				return err
			}
			body.Reset()
			body.i32(login)
			return server.sendFrame(uriLoginRes, body.Bytes())
		}()
	}()
	err := client.login(ctx)
	if serverErr := <-done; serverErr != nil {
		t.Fatal(serverErr)
	}
	if !errors.Is(err, errProtocolRejected) {
		t.Fatalf("expected a protocol rejection, got %v", err)
	}
	return err
}

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
