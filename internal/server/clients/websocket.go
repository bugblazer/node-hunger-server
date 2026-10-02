package clients

import (
	"fmt"
	"log"
	"net/http"
	"sync"

	"server/internal/server"
	"server/internal/server/states"
	"server/pkg/packets"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

// A message waiting to be handled by this client's state.
type inboxItem struct {
	senderId uint64
	msg      packets.Msg
}

// WebSocketClient handles every message on ONE goroutine (processLoop). Messages
// arrive from two places at once: this client's read pump, and the hub delivering
// other players' broadcasts. Before, both called state.HandleMessage directly, so
// the state was read and replaced from different goroutines with no lock, and a
// broadcast landing while the client disconnected hit a nil state and crashed the
// whole server. Now both just queue the message, and only processLoop ever
// touches the state.
type WebSocketClient struct {
	id       uint64
	conn     *websocket.Conn
	hub      *server.Hub
	sendChan chan *packets.Packet
	inbox    chan inboxItem
	state    server.ClientStateHandler // owned by processLoop
	logger   *log.Logger
	dbTx     *server.DbTx

	closing   chan struct{} // closed once, when the client starts shutting down
	closeOnce sync.Once
	loopDone  chan struct{} // closed when processLoop has exited
}

func NewWebSocketClient(hub *server.Hub, writer http.ResponseWriter, request *http.Request) (server.ClientInterfacer, error) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(_ *http.Request) bool { return true },
	}

	conn, err := upgrader.Upgrade(writer, request, nil)

	if err != nil {
		return nil, err
	}

	c := &WebSocketClient{
		hub:      hub,
		conn:     conn,
		sendChan: make(chan *packets.Packet, 256),
		inbox:    make(chan inboxItem, 512),
		logger:   log.New(log.Writer(), "Client unknown: ", log.LstdFlags),
		dbTx:     hub.NewDbTx(),
		closing:  make(chan struct{}),
		loopDone: make(chan struct{}),
	}

	return c, nil
}

func (c *WebSocketClient) Id() uint64 {
	return c.id
}

func (c *WebSocketClient) SetState(state server.ClientStateHandler) {
	prevStateName := "None"
	if c.state != nil {
		prevStateName = c.state.Name()
		c.state.OnExit()
	}

	newStateName := "None"
	if state != nil {
		newStateName = state.Name()
	}

	c.logger.Printf("Switching from state %s to %s", prevStateName, newStateName)

	c.state = state

	if c.state != nil {
		c.state.SetClient(c)
		c.state.OnEnter()
	}
}

// ProcessMessage queues a message for this client's state. It never blocks: the
// hub calls it for every client on every broadcast, so one slow client must not
// hold everyone else up. If the inbox is full the message is dropped, the same
// policy the outgoing sendChan already uses.
func (c *WebSocketClient) ProcessMessage(senderId uint64, message packets.Msg) {
	select {
	case <-c.closing:
		return
	default:
	}
	select {
	case c.inbox <- inboxItem{senderId, message}:
	default:
		c.logger.Printf("Inbox full, dropping message: %T", message)
	}
}

// processLoop is the only goroutine that calls state methods (HandleMessage,
// SetState and therefore OnEnter/OnExit) after Initialize.
func (c *WebSocketClient) processLoop() {
	defer close(c.loopDone)
	for {
		select {
		case <-c.closing:
			c.SetState(nil) // OnExit: remove the player, save their best score
			return
		case item := <-c.inbox:
			if c.state != nil {
				c.state.HandleMessage(item.senderId, item.msg)
			}
		}
	}
}

func (c *WebSocketClient) Initialize(id uint64) {
	c.id = id
	c.logger.SetPrefix(fmt.Sprintf("Client %d: ", c.id))
	c.SetState(&states.Connected{}) // runs before processLoop starts, so no race
	go c.processLoop()
}

func (c *WebSocketClient) SocketSend(message packets.Msg) {
	c.SocketSendAs(message, c.id)
}

func (c *WebSocketClient) SocketSendAs(message packets.Msg, senderId uint64) {
	select {
	case <-c.closing:
		return
	default:
	}
	select {
	case c.sendChan <- &packets.Packet{SenderId: senderId, Msg: message}:
	default:
		c.logger.Printf("Send channel full, dropping message: %T", message)
	}
}

func (c *WebSocketClient) PassToPeer(message packets.Msg, peerId uint64) {
	if peer, exists := c.hub.Clients.Get(peerId); exists {
		peer.ProcessMessage(c.id, message)
	}
}

func (c *WebSocketClient) Broadcast(message packets.Msg) {
	c.hub.BroadcastChan <- &packets.Packet{SenderId: c.id, Msg: message}
}

func (c *WebSocketClient) ReadPump() {
	defer func() {
		c.logger.Println("Closing read pump")
		c.Close("read pump closed")
	}()

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.logger.Printf("Error: %v", err)
			}
			break
		}

		packet := &packets.Packet{}
		err = proto.Unmarshal(data, packet)
		if err != nil {
			c.logger.Printf("error unmarshalling data: %v", err)
			continue
		}

		// To allow the client to lazily not send the sender ID, we'll assume they want to send it as themselves
		if packet.SenderId == 0 {
			packet.SenderId = c.id
		}

		c.ProcessMessage(packet.SenderId, packet.Msg)
	}
}

func (c *WebSocketClient) WritePump() {
	defer func() {
		c.logger.Println("Closing write pump")
		c.Close("write pump closed")
	}()

	for {
		var packet *packets.Packet
		select {
		case <-c.closing:
			return
		case packet = <-c.sendChan:
		}

		writer, err := c.conn.NextWriter(websocket.BinaryMessage)
		if err != nil {
			c.logger.Printf("error getting writer for %T packet, closing client: %v", packet.Msg, err)
			return
		}

		data, err := proto.Marshal(packet)
		if err != nil {
			c.logger.Printf("error marshalling %T packet, closing client: %v", packet.Msg, err)
			continue
		}

		_, err = writer.Write(data)
		if err != nil {
			c.logger.Printf("error writing %T packet: %v", packet.Msg, err)
			continue
		}

		writer.Write([]byte{'\n'})

		if err = writer.Close(); err != nil {
			c.logger.Printf("error closing writer for %T packet: %v", packet.Msg, err)
			continue
		}
	}
}

func (c *WebSocketClient) DbTx() *server.DbTx {
	return c.dbTx
}

func (c *WebSocketClient) SharedGameObjects() *server.SharedGameObjects {
	return c.hub.SharedGameObjects
}

// Close runs once, whichever pump notices the disconnect first. The old version
// ran from both pumps, read from sendChan (blocking when it was empty) and closed
// it, so a late broadcast could panic with "send on closed channel". sendChan is
// now never closed; the pumps and senders stop on c.closing instead.
func (c *WebSocketClient) Close(reason string) {
	c.closeOnce.Do(func() {
		c.logger.Printf("Closing client connection because: %s", reason)

		// Leave the hub first so no new broadcasts are routed to this client.
		c.hub.UnregisterChan <- c
		c.Broadcast(packets.NewDisconnect(reason))

		close(c.closing)
		<-c.loopDone // the player is removed and their score saved before we return
		c.conn.Close()
	})
}
