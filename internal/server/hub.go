package server

import (
	"context"
	"database/sql"
	_ "embed"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path"
	"server/internal/server/db"
	"server/internal/server/objects"
	"server/pkg/packets"
	"time"

	"github.com/tursodatabase/libsql-client-go/libsql"
	_ "modernc.org/sqlite"
)

const MaxSpores = 1000

// Interest management. Every player's position goes out 20 times a second, and
// sending all of them to everyone is 20 x n^2 messages: fine at 30 players, too
// much at 60+. Players you can see still get every update. Players further away
// get one in farUpdateEvery; the client keeps them moving on their last heading
// in between, and they stay in the live leaderboard and chat.
const (
	minViewDistance = 1000.0 // world units; a new player sees about 576 x 324
	viewPerRadius   = 15.0   // the camera zooms out as you grow (about 11.5 x radius half-width)
	farUpdateEvery  = 10     // far players: 2 updates a second instead of 20
)

//go:embed db/config/schema.sql
var schemaGenSql string

type DbTx struct {
	Ctx     context.Context
	Queries *db.Queries
	DB      *sql.DB // for work that must happen in one transaction
}

func (h *Hub) NewDbTx() *DbTx {
	return &DbTx{
		Ctx:     context.Background(),
		Queries: db.New(h.dbPool),
		DB:      h.dbPool,
	}
}

type SharedGameObjects struct {
	// The ID of the player is the ID of the client that owns it
	Players *objects.SharedCollection[*objects.Player]
	Spores  *objects.SharedCollection[*objects.Spore]
	Viruses *objects.SharedCollection[*objects.Virus]

	// W throws that hit a virus, waiting for the virus loop (see FeedVirus)
	VirusFeeds chan VirusFeed
}

// A structure for a state machine to process the client's messages
type ClientStateHandler interface {
	Name() string

	// Inject the client into the state handler
	SetClient(client ClientInterfacer)

	OnEnter()
	HandleMessage(senderId uint64, message packets.Msg)

	// Cleanup the state handler and perform any last actions
	OnExit()
}

type ClientInterfacer interface {
	Id() uint64
	ProcessMessage(senderId uint64, message packets.Msg)

	// Sets the client's ID and anything else that needs to be initialized
	Initialize(id uint64)

	SetState(newState ClientStateHandler)

	// Puts data from this client into the write pump
	SocketSend(message packets.Msg)

	// Puts data from another client into the write pump
	SocketSendAs(message packets.Msg, senderId uint64)

	// Forward message to another client for processing
	PassToPeer(message packets.Msg, peerId uint64)

	// Forward message to all other clients for processing
	Broadcast(message packets.Msg)

	// Pump data from the connected socket directly to the client
	ReadPump()

	// Pump data from the client directly to the connected socket
	WritePump()

	// A reference to the database transaction context for this client
	DbTx() *DbTx

	SharedGameObjects() *SharedGameObjects

	// Close the client's connections and cleanup
	Close(reason string)
}

// The hub is the central point of communication between all connected clients
type Hub struct {
	Clients *objects.SharedCollection[ClientInterfacer]

	// Packets in this channel will be processed by all connected clients except the sender
	BroadcastChan chan *packets.Packet

	// Clients in this channel will be registered to the hub
	RegisterChan chan ClientInterfacer

	// Clients in this channel will be unregistered from the hub
	UnregisterChan chan ClientInterfacer

	// Database connection pool
	dbPool *sql.DB

	SharedGameObjects *SharedGameObjects

	// Per-sender count of position broadcasts, to pick which ones reach far players.
	// Only touched by the hub goroutine, so no lock is needed.
	playerUpdateCount map[uint64]uint64
}

func NewHub(dataDirPath string) *Hub {
	dbPool, err := openDatabase(dataDirPath)
	if err != nil {
		log.Fatalf("Error opening database: %v", err)
	}

	return &Hub{
		Clients:        objects.NewSharedCollection[ClientInterfacer](),
		BroadcastChan:  make(chan *packets.Packet),
		RegisterChan:   make(chan ClientInterfacer),
		UnregisterChan: make(chan ClientInterfacer),
		dbPool:            dbPool,
		playerUpdateCount: make(map[uint64]uint64),
		SharedGameObjects: &SharedGameObjects{
			Players:    objects.NewSharedCollection[*objects.Player](),
			Spores:     objects.NewSharedCollection[*objects.Spore](),
			Viruses:    objects.NewSharedCollection[*objects.Virus](),
			VirusFeeds: make(chan VirusFeed, 64),
		},
	}
}

// openDatabase uses Turso when TURSO_DATABASE_URL is set (the hosted server: free
// hosts wipe their disk on every deploy, which reset everyone's accounts), and a
// local SQLite file otherwise. Both speak SQLite, so the queries are the same.
func openDatabase(dataDirPath string) (*sql.DB, error) {
	if dbUrl := os.Getenv("TURSO_DATABASE_URL"); dbUrl != "" {
		var opts []libsql.Option
		if token := os.Getenv("TURSO_AUTH_TOKEN"); token != "" {
			opts = append(opts, libsql.WithAuthToken(token))
		}
		connector, err := libsql.NewConnector(dbUrl, opts...)
		if err != nil {
			return nil, err
		}
		if u, err := url.Parse(dbUrl); err == nil {
			log.Printf("Using the Turso database at %s", u.Host)
		}
		return sql.OpenDB(connector), nil
	}

	// WAL lets reads continue while a write is in progress, and busy_timeout makes a
	// writer wait for the lock instead of failing at once. Without them, a few
	// players signing up at the same moment got "database is locked" (SQLITE_BUSY).
	// _txlock=immediate takes the write lock when a transaction starts, so two
	// transactions can't both read and then deadlock trying to upgrade.
	dsn := "file:" + path.Join(dataDirPath, "db.sqlite") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate"
	log.Printf("Using the local SQLite database in %s", dataDirPath)
	return sql.Open("sqlite", dsn)
}

func (h *Hub) Run() {
	log.Println("Initializing database...")
	if _, err := h.dbPool.ExecContext(context.Background(), schemaGenSql); err != nil {
		log.Fatalf("Error initializing database: %v", err)
	}

	log.Println("Placing spores...")
	for i := 0; i < MaxSpores; i++ {
		h.SharedGameObjects.Spores.Add(h.newSpore())
	}

	go h.replenishSporesLoop(2 * time.Second)

	log.Println("Placing viruses...")
	h.placeViruses()
	go h.virusLoop()

	log.Println("Awaiting client registrations")
	for {
		select {
		case client := <-h.RegisterChan:
			client.Initialize(h.Clients.Add(client))
		case client := <-h.UnregisterChan:
			h.Clients.Remove(client.Id())
			delete(h.playerUpdateCount, client.Id())
		case packet := <-h.BroadcastChan:
			h.broadcast(packet)
		}
	}
}

// broadcast delivers a packet to every client except its sender. Position
// updates (Player packets) are thinned out for players who are far away.
func (h *Hub) broadcast(packet *packets.Packet) {
	playerMsg, isPlayerUpdate := packet.Msg.(*packets.Packet_Player)
	sendToFar := true
	if isPlayerUpdate {
		n := h.playerUpdateCount[packet.SenderId]
		h.playerUpdateCount[packet.SenderId] = n + 1
		sendToFar = n%farUpdateEvery == 0
	}

	h.Clients.ForEach(func(clientId uint64, client ClientInterfacer) {
		if clientId == packet.SenderId {
			return
		}
		if isPlayerUpdate && !sendToFar && !h.canSee(clientId, playerMsg.Player) {
			return
		}
		client.ProcessMessage(packet.SenderId, packet.Msg)
	})
}

// canSee reports whether the viewer's player is close enough to the given player
// to have them on screen. Viewers who aren't in the game see everything.
func (h *Hub) canSee(viewerId uint64, other *packets.PlayerMessage) bool {
	viewer, inGame := h.SharedGameObjects.Players.Get(viewerId)
	if !inGame {
		return true
	}
	reach := max(minViewDistance, viewPerRadius*viewer.Radius) + other.Radius
	dx, dy := viewer.X-other.X, viewer.Y-other.Y
	return dx*dx+dy*dy <= reach*reach
}

func (h *Hub) Serve(getNewClient func(*Hub, http.ResponseWriter, *http.Request) (ClientInterfacer, error), writer http.ResponseWriter, request *http.Request) {
	log.Println("New client connected from", request.RemoteAddr)
	client, err := getNewClient(h, writer, request)

	if err != nil {
		log.Printf("Error obtaining client for new connection: %v", err)
		return
	}

	h.RegisterChan <- client

	go client.WritePump()
	go client.ReadPump()
}

func (h *Hub) newSpore() *objects.Spore {
	sporeRadius := max(10+rand.NormFloat64()*3, 5)
	x, y := objects.SpawnCoords(sporeRadius, h.SharedGameObjects.Players, h.SharedGameObjects.Spores)
	return &objects.Spore{X: x, Y: y, Radius: sporeRadius}
}

func (h *Hub) replenishSporesLoop(rate time.Duration) {
	ticker := time.NewTicker(rate)
	defer ticker.Stop()

	for range ticker.C {
		sporesRemaining := h.SharedGameObjects.Spores.Len()
		diff := MaxSpores - sporesRemaining

		if diff <= 0 {
			continue
		}

		log.Printf("%d spores remain - going to replenish %d spores", sporesRemaining, diff)

		// Don't really want to spawn too many at a time, otherwise it can cause lag spikes
		for i := 0; i < min(diff, 10); i++ {
			spore := h.newSpore()
			sporeId := h.SharedGameObjects.Spores.Add(spore)

			h.BroadcastChan <- &packets.Packet{
				SenderId: 0,
				Msg:      packets.NewSpore(sporeId, spore),
			}

			// Sleep a little bit to avoid lag spikes
			time.Sleep(50 * time.Millisecond)
		}
	}
}
