package bigolistener

import (
	"context"
	"crypto/rc4"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"
)

const testQueryRequestURI, testQueryResponseURI = 900001, 900002

func queryTestConnection(t *testing.T) (*Hub, *linkdClient, *linkdClient) {
	t.Helper()
	first, second := net.Pipe()
	cipher := func() *rc4.Cipher {
		result, err := rc4.NewCipher([]byte("synthetic-query-key"))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	client := &linkdClient{conn: first, sendCipher: cipher(), recvCipher: cipher(), session: validTestSession()}
	server := &linkdClient{conn: second, sendCipher: cipher(), recvCipher: cipher()}
	hub := &Hub{activeClient: client, status: RuntimeStatus{State: "connected", Connected: true}}
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return hub, client, server
}

func queryTestBody(sequence uint32) []byte {
	body := make([]byte, 4)
	binary.LittleEndian.PutUint32(body, sequence)
	return body
}

func queryTestMatch(sequence uint32) func([]byte) bool {
	return func(body []byte) bool {
		return len(body) >= 4 && binary.LittleEndian.Uint32(body) == sequence
	}
}

func queryTestPush() []byte {
	body := &wireWriter{}
	body.i32(protocolAppID)
	body.u32(77)
	return body.Bytes()
}

func TestSharedQueriesKeepSetupPushAndHeartbeatOnOneConnection(t *testing.T) {
	t.Parallel()
	hub, client, server := queryTestConnection(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	hub.dial = func(context.Context, Session) (hubClient, string, error) {
		return nil, "", errors.New("query opened a second connection")
	}
	setupDone := make(chan error, 1)
	go func() {
		_, err := client.awaitURISequence(ctx, uriAddFollowRes, 4, 42)
		setupDone <- err
	}()
	results := make(chan error, 2)
	for _, sequence := range []uint32{11, 22} {
		go func() {
			frame, err := hub.QueryAccount(ctx, testQueryRequestURI, testQueryResponseURI, func(uid uint32) ([]byte, func([]byte) bool, error) {
				if uid != client.session.UID {
					return nil, nil, errors.New("wrong account for shared query")
				}
				return queryTestBody(sequence), queryTestMatch(sequence), nil
			})
			if err == nil && !queryTestMatch(sequence)(frame.Body) {
				err = errors.New("query received another request's response")
			}
			results <- err
		}()
	}
	pingDone := make(chan error, 1)
	go func() { pingDone <- client.sendPing() }()
	var sequences []uint32
	sawPing := false
	for range 3 {
		frame, err := server.readFrame(ctx)
		if err != nil {
			t.Fatal(err)
		}
		switch frame.URI {
		case testQueryRequestURI:
			sequences = append(sequences, binary.LittleEndian.Uint32(frame.Body))
		case uriPing:
			sawPing = true
		default:
			t.Fatalf("unexpected outgoing URI %d", frame.URI)
		}
	}
	if len(sequences) != 2 || !sawPing {
		t.Fatal("queries prevented the heartbeat")
	}
	for _, frame := range []linkdFrame{
		{URI: testQueryResponseURI, Body: queryTestBody(sequences[1])},
		{URI: uriNewLiveNotify, Body: queryTestPush()},
		{URI: uriPing},
		{URI: testQueryResponseURI, Body: queryTestBody(sequences[0])},
	} {
		if err := server.sendFrame(frame.URI, frame.Body); err != nil {
			t.Fatal(err)
		}
	}
	setup := &wireWriter{}
	setup.i32(protocolAppID)
	setup.u32(42)
	setup.u32(client.session.UID)
	setup.i32(0)
	if err := server.sendFrame(uriAddFollowRes, setup.Bytes()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if err := <-pingDone; err != nil {
		t.Fatal(err)
	}
	if err := <-setupDone; err != nil {
		t.Fatal(err)
	}
	if len(client.pendingEvents) != 1 || client.pendingEvents[0].URI != uriNewLiveNotify {
		t.Fatal("setup discarded the interleaved live notification")
	}
	if status := hub.Status(); !status.Connected || status.State != "connected" {
		t.Fatalf("queries changed the listener status: %+v", status)
	}
}

func TestCanceledQueryIgnoresLateReplyDuringNextQuery(t *testing.T) {
	t.Parallel()
	hub, client, server := queryTestConnection(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	readerDone := make(chan error, 1)
	go func() { _, err := client.waitLiveEvent(ctx); readerDone <- err }()
	firstCtx, cancelFirst := context.WithCancel(ctx)
	firstDone := make(chan error, 1)
	go func() {
		_, err := hub.QueryMatch(firstCtx, testQueryRequestURI, testQueryResponseURI, queryTestBody(11), queryTestMatch(11))
		firstDone <- err
	}()
	if _, err := server.readFrame(ctx); err != nil {
		t.Fatal(err)
	}
	cancelFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query returned %v", err)
	}
	secondDone := make(chan error, 1)
	go func() {
		frame, err := hub.QueryMatch(ctx, testQueryRequestURI, testQueryResponseURI, queryTestBody(22), queryTestMatch(22))
		if err == nil && !queryTestMatch(22)(frame.Body) {
			err = errors.New("late response completed the next query")
		}
		secondDone <- err
	}()
	if _, err := server.readFrame(ctx); err != nil {
		t.Fatal(err)
	}
	for _, sequence := range []uint32{11, 22} {
		if err := server.sendFrame(testQueryResponseURI, queryTestBody(sequence)); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if err := server.sendFrame(uriNewLiveNotify, queryTestPush()); err != nil {
		t.Fatal(err)
	}
	if err := <-readerDone; err != nil {
		t.Fatalf("canceled query interrupted event delivery: %v", err)
	}
}

func TestConnectionCloseFailsPendingQueriesImmediately(t *testing.T) {
	t.Parallel()
	hub, client, server := queryTestConnection(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	queryDone := make(chan error, 1)
	go func() {
		_, err := hub.QueryMatch(ctx, testQueryRequestURI, testQueryResponseURI, queryTestBody(11), queryTestMatch(11))
		queryDone <- err
	}()
	if _, err := server.readFrame(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-queryDone; !errors.Is(err, ErrNoConnection) {
		t.Fatalf("closed connection returned %v", err)
	}
	if _, err := hub.Query(ctx, testQueryRequestURI, testQueryResponseURI, nil); !errors.Is(err, ErrNoConnection) {
		t.Fatalf("closed connection accepted a new query: %v", err)
	}
}

func TestQueryWriteHonorsCancellation(t *testing.T) {
	t.Parallel()
	for _, heldWriter := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocked socket", true: "queued writer"}[heldWriter], func(t *testing.T) {
			hub, client, _ := queryTestConnection(t)
			if heldWriter {
				client.writeOnce.Do(func() { client.writeGate = make(chan struct{}, 1) })
				client.writeGate <- struct{}{}
				defer func() { <-client.writeGate }()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if _, err := hub.QueryMatch(ctx, testQueryRequestURI, testQueryResponseURI, queryTestBody(11), queryTestMatch(11)); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("write cancellation returned %v", err)
			}
			client.queryMu.Lock()
			pending := len(client.queries)
			client.queryMu.Unlock()
			if pending != 0 {
				t.Fatal("canceled write retained a pending query")
			}
		})
	}
}

func TestQueryAccountKeepsOneConnectionAcrossReconfiguration(t *testing.T) {
	t.Parallel()
	hub, client, server := queryTestConnection(t)
	_, replacement, _ := queryTestConnection(t)
	replacement.session.UID = 456
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	readerDone := make(chan error, 1)
	go func() { _, err := client.waitLiveEvent(ctx); readerDone <- err }()
	queryDone := make(chan error, 1)
	go func() {
		_, err := hub.QueryAccount(ctx, testQueryRequestURI, testQueryResponseURI, func(uid uint32) ([]byte, func([]byte) bool, error) {
			hub.activeClientMu.Lock()
			hub.activeClient = replacement
			hub.activeClientMu.Unlock()
			return queryTestBody(uid), queryTestMatch(uid), nil
		})
		queryDone <- err
	}()
	request, err := server.readFrame(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !queryTestMatch(client.session.UID)(request.Body) {
		t.Fatal("query mixed the replacement account with the original connection")
	}
	if err := server.sendFrame(testQueryResponseURI, request.Body); err != nil {
		t.Fatal(err)
	}
	if err := <-queryDone; err != nil {
		t.Fatal(err)
	}
	if err := server.sendFrame(uriNewLiveNotify, queryTestPush()); err != nil {
		t.Fatal(err)
	}
	if err := <-readerDone; err != nil {
		t.Fatal(err)
	}
}

func TestQueryRejectsMissingConnectionAndCanceledContext(t *testing.T) {
	t.Parallel()
	hub := &Hub{}
	if _, err := hub.Query(context.Background(), testQueryRequestURI, testQueryResponseURI, nil); !errors.Is(err, ErrNoConnection) {
		t.Fatalf("disconnected hub returned %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hub.Query(ctx, testQueryRequestURI, testQueryResponseURI, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query returned %v", err)
	}
}

func TestUncorrelatedCanceledQueryDiscardsReplyBeforeReuse(t *testing.T) {
	t.Parallel()
	hub, client, server := queryTestConnection(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	readerDone := make(chan error, 1)
	go func() { _, err := client.waitLiveEvent(ctx); readerDone <- err }()
	firstCtx, cancelFirst := context.WithCancel(ctx)
	queryDone := make(chan error, 1)
	go func() {
		_, err := hub.Query(firstCtx, testQueryRequestURI, testQueryResponseURI, queryTestBody(11))
		queryDone <- err
	}()
	if _, err := server.readFrame(ctx); err != nil {
		t.Fatal(err)
	}
	cancelFirst()
	if err := <-queryDone; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := hub.Query(ctx, testQueryRequestURI, testQueryResponseURI, queryTestBody(22)); err == nil {
		t.Fatal("uncorrelated late reply could be accepted by the next request")
	}
	if err := server.sendFrame(testQueryResponseURI, queryTestBody(11)); err != nil {
		t.Fatal(err)
	}
	if err := server.sendFrame(uriNewLiveNotify, queryTestPush()); err != nil {
		t.Fatal(err)
	}
	if err := <-readerDone; err != nil {
		t.Fatal(err)
	}
	client.queryMu.Lock()
	pending := len(client.queries)
	client.queryMu.Unlock()
	if pending != 0 {
		t.Fatal("uncorrelated canceled reply was not discarded")
	}
}
