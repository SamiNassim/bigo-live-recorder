package bigolistener

import (
	"context"
	"crypto/rc4"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

type fakeHubClient struct {
	rejectFollow map[uint32]error
	followCalls  []uint32
	noticeCalls  []uint32
	waitErr      error
}

type dynamicHubClient struct {
	follows chan uint32
	notices chan uint32
	waits   chan struct{}
}

func (c *dynamicHubClient) Close() error { return nil }

func (c *dynamicHubClient) addFollow(_ context.Context, uid uint32) error {
	c.follows <- uid
	return nil
}

func (c *dynamicHubClient) enableLiveNotice(_ context.Context, uid uint32) error {
	c.notices <- uid
	return nil
}

func (c *dynamicHubClient) waitLiveEvent(ctx context.Context) (liveEvent, error) {
	c.waits <- struct{}{}
	<-ctx.Done()
	return liveEvent{}, ctx.Err()
}

func (c *fakeHubClient) Close() error { return nil }

func (c *fakeHubClient) addFollow(_ context.Context, uid uint32) error {
	c.followCalls = append(c.followCalls, uid)
	return c.rejectFollow[uid]
}

func (c *fakeHubClient) enableLiveNotice(_ context.Context, uid uint32) error {
	c.noticeCalls = append(c.noticeCalls, uid)
	return nil
}

func (c *fakeHubClient) waitLiveEvent(context.Context) (liveEvent, error) {
	return liveEvent{}, c.waitErr
}

func TestHubDispatchTargetsMatchingStreamerAndBroadcastsFallback(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	hub := NewHub(ctx, testSessionSource, nil, nil)
	defer func() {
		cancel()
		hub.Close()
	}()

	first, removeFirst, err := hub.Subscribe("public-one", 111)
	if err != nil {
		t.Fatal(err)
	}
	defer removeFirst()
	second, removeSecond, err := hub.Subscribe("public-two", 222)
	if err != nil {
		t.Fatal(err)
	}
	defer removeSecond()

	hub.dispatch(noticeIdentity{uid: 222})
	select {
	case notification := <-second:
		if !notification.Attributed {
			t.Fatal("matching target notification was marked unattributed")
		}
	default:
		t.Fatal("matching target was not notified")
	}
	select {
	case <-first:
		t.Fatal("unrelated target was notified")
	default:
	}

	hub.dispatch(noticeIdentity{})
	for name, events := range map[string]<-chan Notification{"first": first, "second": second} {
		select {
		case notification := <-events:
			if notification.Attributed {
				t.Fatalf("%s terse event was incorrectly marked as target-attributed", name)
			}
		default:
			t.Fatalf("%s target was not notified by terse event", name)
		}
	}
}

func TestLiveMessageID(t *testing.T) {
	t.Parallel()
	content := `{"key_reserved":"{\"seqid\":\"987654\"}"}`
	if got := liveMessageID(content); got != 987654 {
		t.Fatalf("unexpected message id %d", got)
	}
	if got := liveMessageID(strings.Repeat("x", 8)); got != 0 {
		t.Fatalf("invalid content returned message id %d", got)
	}
	for _, content := range []string{
		`{"key_reserved":{"seqid":"1234567890123456789"}}`,
		`{"key_reserved":{"seqid":1234567890123456789}}`,
		`{"key_reserved":"{\"seqid\":\"1234567890123456789\"}"}`,
	} {
		if got := liveMessageID(content); got != 1234567890123456789 {
			t.Fatalf("message ID lost precision or object form: %d", got)
		}
	}
}

func TestAlreadyActiveFollowResultAllowsNotificationSetup(t *testing.T) {
	t.Parallel()
	for result, want := range map[int32]bool{
		0:                           true,
		protocolFollowAlreadyActive: true,
		9:                           false,
	} {
		if got := followResultAccepted(result); got != want {
			t.Fatalf("follow result %d accepted=%t, want %t", result, got, want)
		}
	}
}

func TestConfigureKeepsNativeModeSelectedWhenSessionIsUnusable(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	warnings := make([]string, 0, 1)
	hub := NewHub(ctx, testSessionSource, nil, func(message string) { warnings = append(warnings, message) })
	defer func() {
		cancel()
		hub.Close()
	}()

	if !hub.Configure(true, "", "device") {
		t.Fatal("native mode should remain selected with an empty session")
	}
	status := hub.Status()
	if status.State != "error" || status.Connected {
		t.Fatalf("status=%+v, want a disconnected configuration error", status)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "native listening is inactive") {
		t.Fatalf("warnings=%v, want explicit push-only warning", warnings)
	}
	if hub.Configure(false, "", "device") {
		t.Fatal("disabling native mode should select regular polling")
	}
	status = hub.Status()
	if status.State != "disabled" || status.Connected {
		t.Fatalf("status=%+v, want disabled", status)
	}
}

func TestHubConnectedStatusReportsSubscriptionReadiness(t *testing.T) {
	t.Parallel()
	hub := &Hub{ctx: context.Background(), watchers: make(map[uint64]*watcher)}

	hub.setConnectedStatus(2, 2)
	status := hub.Status()
	if status.State != "connected" || !status.Connected || status.ReadyTargets != 2 || status.TotalTargets != 2 {
		t.Fatalf("status=%+v, want fully connected", status)
	}

	hub.setConnectedStatus(1, 2)
	status = hub.Status()
	if status.State != "partial" || !status.Connected || status.ReadyTargets != 1 || status.TotalTargets != 2 {
		t.Fatalf("status=%+v, want partially connected", status)
	}

	hub.setConnectedStatus(0, 2)
	status = hub.Status()
	if status.State != "unavailable" || !status.Connected || status.ReadyTargets != 0 || status.TotalTargets != 2 {
		t.Fatalf("status=%+v, want connected with unavailable subscriptions", status)
	}

	hub.reportConnectionError(errors.New("test disconnect"))
	status = hub.Status()
	if status.State != "retrying" || status.Connected {
		t.Fatalf("status=%+v, want disconnected and retrying", status)
	}
}

func TestAwaitURISequenceIgnoresStaleResponseAndPreservesPush(t *testing.T) {
	t.Parallel()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	key := []byte("bigo-test-key")
	recvCipher, err := rc4.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	sendCipher, err := rc4.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	client := &linkdClient{conn: clientConn, recvCipher: recvCipher}

	const wantSequence uint32 = 9000
	responseBody := func(sequence uint32) []byte {
		body := &wireWriter{}
		body.i32(protocolAppID)
		body.u32(sequence)
		body.u32(123)
		body.i32(0)
		return body.Bytes()
	}
	pushBody := &wireWriter{}
	pushBody.i32(protocolAppID)
	pushBody.u32(77)
	frames := [][]byte{
		marshalFrame(uriAddFollowRes, responseBody(8000)),
		marshalFrame(uriNewLiveNotify, pushBody.Bytes()),
		marshalFrame(uriAddFollowRes, responseBody(wantSequence)),
	}
	go func() {
		for _, frame := range frames {
			encrypted := make([]byte, len(frame))
			sendCipher.XORKeyStream(encrypted, frame)
			if writeAll(serverConn, encrypted) != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := client.awaitURISequence(ctx, uriAddFollowRes, 4, wantSequence)
	if err != nil {
		t.Fatalf("await matching response: %v", err)
	}
	reader := &wireReader{data: response.Body}
	_, _ = reader.i32()
	gotSequence, err := reader.i32()
	if err != nil || uint32(gotSequence) != wantSequence {
		t.Fatalf("response sequence=%d err=%v, want %d", uint32(gotSequence), err, wantSequence)
	}
	if len(client.pendingEvents) != 1 || client.pendingEvents[0].URI != uriNewLiveNotify {
		t.Fatalf("pending events=%+v, want preserved new-live push", client.pendingEvents)
	}
}

func TestGetWebAuthTokenUsesOfficialProtocol(t *testing.T) {
	t.Parallel()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	key := []byte("bigo-web-auth-test")
	clientSendCipher, err := rc4.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	clientRecvCipher, err := rc4.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	serverRecvCipher, err := rc4.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	serverSendCipher, err := rc4.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	client := &linkdClient{
		conn:       clientConn,
		sendCipher: clientSendCipher,
		recvCipher: clientRecvCipher,
		seq:        41,
	}

	serverErr := make(chan error, 1)
	go func() {
		header := make([]byte, 4)
		if _, err := io.ReadFull(serverConn, header); err != nil {
			serverErr <- err
			return
		}
		serverRecvCipher.XORKeyStream(header, header)
		length := int(binary.LittleEndian.Uint32(header))
		rest := make([]byte, length-4)
		if _, err := io.ReadFull(serverConn, rest); err != nil {
			serverErr <- err
			return
		}
		serverRecvCipher.XORKeyStream(rest, rest)
		frame, err := parseFrame(append(header, rest...))
		if err != nil {
			serverErr <- err
			return
		}
		if frame.URI != uriGetAuthToken {
			serverErr <- fmt.Errorf("request uri=%d, want %d", frame.URI, uriGetAuthToken)
			return
		}
		reader := &wireReader{data: frame.Body}
		appID, _ := reader.i32()
		sequence, _ := reader.i32()
		pageURL, _ := reader.string()
		extra, _ := reader.string()
		if appID != protocolAppID || sequence != 42 || pageURL != "https://www.bigo.tv/" || extra != "" {
			serverErr <- fmt.Errorf("unexpected request app=%d seq=%d url=%q extra=%q", appID, sequence, pageURL, extra)
			return
		}

		body := &wireWriter{}
		body.i32(protocolAppID)
		body.i32(sequence)
		body.string("native-token")
		body.i32(300)
		body.i32(0)
		body.i32(123456)
		response := marshalFrame(uriGetAuthTokenRes, body.Bytes())
		encrypted := make([]byte, len(response))
		serverSendCipher.XORKeyStream(encrypted, response)
		serverErr <- writeAll(serverConn, encrypted)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	token, err := client.getWebAuthToken(ctx, "https://www.bigo.tv/")
	if err != nil {
		t.Fatal(err)
	}
	if token != "native-token" {
		t.Fatalf("web token=%q, want Linkd-issued token", token)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestHubIsolatesRejectedTargetAndRetainsConfirmedSetup(t *testing.T) {
	t.Parallel()
	firstClient := &fakeHubClient{
		rejectFollow: map[uint32]error{
			111: fmt.Errorf("%w: follow result=9", errProtocolRejected),
		},
	}
	warnings := make([]string, 0, 1)
	hub := &Hub{
		warning: func(message string) { warnings = append(warnings, message) },
	}
	targets := []target{
		{publicID: "rejected", uid: 111},
		{publicID: "working", uid: 222},
	}
	followed := make(map[uint32]bool)
	subscribed := make(map[uint32]bool)
	rejected := make(map[uint32]targetRejection)

	ready, err := hub.setupTargets(context.Background(), firstClient, targets, followed, subscribed, rejected)
	if err != nil || ready != 1 {
		t.Fatalf("setup ready=%d error=%v, want one working target", ready, err)
	}
	if len(firstClient.followCalls) != 2 || firstClient.followCalls[0] != 111 || firstClient.followCalls[1] != 222 {
		t.Fatalf("follow calls=%v, want rejected target followed by working target", firstClient.followCalls)
	}
	if len(firstClient.noticeCalls) != 1 || firstClient.noticeCalls[0] != 222 {
		t.Fatalf("notice calls=%v, want only working target", firstClient.noticeCalls)
	}
	if !followed[222] || !subscribed[222] || rejected[111].err == nil {
		t.Fatalf("state followed=%v subscribed=%v rejected=%v", followed, subscribed, rejected)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "polling remains disabled") {
		t.Fatalf("warnings=%v, want explicit push-only fallback warning", warnings)
	}

	secondClient := &fakeHubClient{rejectFollow: make(map[uint32]error)}
	ready, err = hub.setupTargets(context.Background(), secondClient, targets, followed, subscribed, rejected)
	if err != nil || ready != 1 {
		t.Fatalf("reconnect setup ready=%d error=%v, want retained working target", ready, err)
	}
	if len(secondClient.followCalls) != 0 || len(secondClient.noticeCalls) != 0 {
		t.Fatalf("reconnect repeated confirmed setup: follows=%v notices=%v", secondClient.followCalls, secondClient.noticeCalls)
	}

	due := rejected[111]
	due.retryAt = time.Now().Add(-time.Second)
	rejected[111] = due
	thirdClient := &fakeHubClient{rejectFollow: make(map[uint32]error)}
	ready, err = hub.setupTargets(context.Background(), thirdClient, targets, followed, subscribed, rejected)
	if err != nil || ready != 2 {
		t.Fatalf("retry setup ready=%d error=%v, want both targets active", ready, err)
	}
	if len(thirdClient.followCalls) != 1 || thirdClient.followCalls[0] != 111 || len(thirdClient.noticeCalls) != 1 || thirdClient.noticeCalls[0] != 111 {
		t.Fatalf("native retry calls follows=%v notices=%v, want only recovered target", thirdClient.followCalls, thirdClient.noticeCalls)
	}
	if _, exists := rejected[111]; exists {
		t.Fatal("successful native retry did not clear target rejection")
	}
}

func TestHubAddsTargetWithoutClosingOrRepeatingExistingSetup(t *testing.T) {
	t.Parallel()
	connectionCtx, cancelConnection := context.WithCancel(context.Background())
	defer cancelConnection()
	client := &dynamicHubClient{
		follows: make(chan uint32, 4),
		notices: make(chan uint32, 4),
		waits:   make(chan struct{}, 4),
	}
	hub := &Hub{
		enabled:   true,
		usable:    true,
		configKey: "test-session",
		source:    testSessionSource,
		watchers: map[uint64]*watcher{
			1: {publicID: "first", targetUID: 111, events: make(chan Notification, 1)},
		},
		nextWatcher: 1,
		version:     1,
		wake:        make(chan struct{}, 1),
		dial: func(context.Context, Session) (hubClient, string, error) {
			return client, "test-linkd", nil
		},
	}
	snapshot, ready := hub.snapshot()
	if !ready {
		t.Fatal("initial hub snapshot is not ready")
	}
	done := make(chan error, 1)
	go func() {
		done <- hub.connectAndListen(
			connectionCtx,
			snapshot,
			make(map[uint32]bool),
			make(map[uint32]bool),
			make(map[uint32]targetRejection),
		)
	}()

	waitForTestUID(t, client.follows, 111, "initial follow")
	waitForTestUID(t, client.notices, 111, "initial notice")
	waitForTestSignal(t, client.waits, "initial event wait")

	_, removeSecond, err := hub.Subscribe("second", 222)
	if err != nil {
		t.Fatal(err)
	}
	defer removeSecond()
	waitForTestUID(t, client.follows, 222, "new target follow")
	waitForTestUID(t, client.notices, 222, "new target notice")
	waitForTestSignal(t, client.waits, "resumed event wait")

	select {
	case duplicate := <-client.follows:
		t.Fatalf("existing target setup repeated for uid %d", duplicate)
	default:
	}
	cancelConnection()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("listener exit error=%v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("listener did not stop after context cancellation")
	}
}

func TestReconnectRefreshesTicketWithoutCreatingPerTargetLogins(t *testing.T) {
	t.Parallel()
	issued := 0
	var usedTickets []string
	client := &fakeHubClient{rejectFollow: map[uint32]error{}, waitErr: io.EOF}
	hub := &Hub{
		enabled: true, usable: true, configKey: "cookie", wake: make(chan struct{}, 1),
		watchers: map[uint64]*watcher{
			1: {publicID: "first", targetUID: 111, events: make(chan Notification, 1)},
			2: {publicID: "second", targetUID: 222, events: make(chan Notification, 1)},
		},
		source: func(context.Context, string) (Session, error) {
			issued++
			s := validTestSession()
			s.Cookie = []byte(fmt.Sprintf("fresh-ticket-%d", issued))
			return s, nil
		},
		dial: func(_ context.Context, s Session) (hubClient, string, error) {
			usedTickets = append(usedTickets, string(s.Cookie))
			return client, "test-linkd", nil
		},
	}
	snapshot, _ := hub.snapshot()
	followed, subscribed := map[uint32]bool{}, map[uint32]bool{}
	rejected := map[uint32]targetRejection{}
	for range 2 {
		if err := hub.connectAndListen(context.Background(), snapshot, followed, subscribed, rejected); !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	}
	if issued != 2 || len(usedTickets) != 2 || usedTickets[0] == usedTickets[1] {
		t.Fatalf("reconnect reused a stale ticket or logged in per target: %v", usedTickets)
	}
	if len(client.followCalls) != 2 || len(client.noticeCalls) != 2 {
		t.Fatal("reconnect repeated confirmed account preferences")
	}
}

func TestCookieChangeCancelsCredentialBootstrap(t *testing.T) {
	t.Parallel()
	entered, stopped := make(chan struct{}), make(chan struct{})
	hub := NewHub(context.Background(), func(ctx context.Context, _ string) (Session, error) {
		close(entered)
		<-ctx.Done()
		close(stopped)
		return Session{}, ctx.Err()
	}, nil, nil)
	defer hub.Close()
	hub.Configure(true, "first-cookie", "device")
	_, unsubscribe, err := hub.Subscribe("target", 123)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("credential bootstrap did not start")
	}
	hub.Configure(false, "new-cookie", "device")
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("configuration change did not cancel old credential request")
	}
	if hub.Status().Connected {
		t.Fatal("disabled listener reported connected")
	}
}

func waitForTestUID(t *testing.T, values <-chan uint32, want uint32, label string) {
	t.Helper()
	select {
	case got := <-values:
		if got != want {
			t.Fatalf("%s uid=%d, want %d", label, got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func waitForTestSignal(t *testing.T, values <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-values:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}
