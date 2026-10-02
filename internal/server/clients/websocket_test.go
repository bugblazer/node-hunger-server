package clients

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"server/internal/server"
	"server/pkg/packets"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

func startServer(t *testing.T) string {
	t.Helper()
	// Not t.TempDir(): the hub keeps db.sqlite open for the life of the process,
	// and Windows refuses to delete an open file, which would fail the test.
	dir, err := os.MkdirTemp("", "nodehunger-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	hub := server.NewHub(dir)
	go hub.Run()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.Serve(NewWebSocketClient, w, r)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

type testConn struct{ *websocket.Conn }

func dial(t *testing.T, url string) testConn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return testConn{c}
}

func (c testConn) send(msg packets.Msg) error {
	b, _ := proto.Marshal(&packets.Packet{Msg: msg})
	return c.WriteMessage(websocket.BinaryMessage, b)
}

func (c testConn) read() (*packets.Packet, error) {
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, b, err := c.ReadMessage()
	if err != nil {
		return nil, err
	}
	b = []byte(strings.TrimSuffix(string(b), "\n")) // the server ends every frame with a newline
	p := &packets.Packet{}
	return p, proto.Unmarshal(b, p)
}

// waitOk reads until an OK (nil error) or a Deny (error with the reason).
func (c testConn) waitOk() error {
	for {
		p, err := c.read()
		if err != nil {
			return err
		}
		switch m := p.Msg.(type) {
		case *packets.Packet_OkResponse:
			return nil
		case *packets.Packet_DenyResponse:
			return fmt.Errorf("denied: %s", m.DenyResponse.Reason)
		}
	}
}

func (c testConn) waitId() error {
	for {
		p, err := c.read()
		if err != nil {
			return err
		}
		if _, ok := p.Msg.(*packets.Packet_Id); ok {
			return nil
		}
	}
}

func joinGame(t *testing.T, url, name string) testConn {
	c := dial(t, url)
	if err := c.waitId(); err != nil {
		t.Errorf("%s: no id: %v", name, err)
		return c
	}
	c.send(&packets.Packet_RegisterRequest{RegisterRequest: &packets.RegisterRequestMessage{Username: name, Password: "test-password", Color: 0xff0000}})
	if err := c.waitOk(); err != nil {
		t.Errorf("%s: register: %v", name, err)
		return c
	}
	c.send(&packets.Packet_LoginRequest{LoginRequest: &packets.LoginRequestMessage{Username: name, Password: "test-password"}})
	if err := c.waitOk(); err != nil {
		t.Errorf("%s: login: %v", name, err)
	}
	c.send(&packets.Packet_PlayerDirection{PlayerDirection: &packets.PlayerDirectionMessage{Direction: 1}})
	return c
}

// Regression test for two production bugs:
//   - concurrent sign-ups failed with SQLITE_BUSY and could leave half-created accounts;
//   - players leaving at the same time crashed the server (nil state / closed channel),
//     which would panic and fail this whole test binary.
func TestManyPlayersJoinTogetherAndLeaveTogether(t *testing.T) {
	url := startServer(t)
	const n = 20

	conns := make([]testConn, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conns[i] = joinGame(t, url, fmt.Sprintf("player%02d", i))
		}(i)
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}

	time.Sleep(500 * time.Millisecond) // let position broadcasts flow between everyone

	for _, c := range conns { // everyone leaves at once
		go c.Close()
	}
	time.Sleep(500 * time.Millisecond)

	// The server must still be up: a new player can connect and sign up.
	c := joinGame(t, url, "afterwards")
	c.Close()
}

func TestTakenNameIsRefusedButTheAccountStillWorks(t *testing.T) {
	url := startServer(t)
	joinGame(t, url, "taken").Close()

	c := dial(t, url)
	defer c.Close()
	c.waitId()
	c.send(&packets.Packet_RegisterRequest{RegisterRequest: &packets.RegisterRequestMessage{Username: "taken", Password: "other-password", Color: 1}})
	if err := c.waitOk(); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected 'already exists', got %v", err)
	}
	c.send(&packets.Packet_LoginRequest{LoginRequest: &packets.LoginRequestMessage{Username: "taken", Password: "test-password"}})
	if err := c.waitOk(); err != nil {
		t.Fatalf("original account should still log in: %v", err)
	}
}
