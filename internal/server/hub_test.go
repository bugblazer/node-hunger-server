package server

import (
	"testing"

	"server/internal/server/objects"
	"server/pkg/packets"
)

// fakeClient records what the hub delivers. Embedding the interface satisfies
// the methods the hub never calls in these tests.
type fakeClient struct {
	ClientInterfacer
	id       uint64
	received []packets.Msg
}

func (f *fakeClient) Id() uint64 { return f.id }
func (f *fakeClient) ProcessMessage(_ uint64, m packets.Msg) {
	f.received = append(f.received, m)
}

func testHub() *Hub {
	return &Hub{
		Clients:           objects.NewSharedCollection[ClientInterfacer](),
		playerUpdateCount: map[uint64]uint64{},
		SharedGameObjects: &SharedGameObjects{
			Players: objects.NewSharedCollection[*objects.Player](),
			Spores:  objects.NewSharedCollection[*objects.Spore](),
		},
	}
}

func addPlayer(h *Hub, id uint64, x, y, radius float64) *fakeClient {
	c := &fakeClient{id: id}
	h.Clients.Add(c, id)
	h.SharedGameObjects.Players.Add(&objects.Player{X: x, Y: y, Radius: radius}, id)
	return c
}

func positionUpdate(sender uint64, x, y float64) *packets.Packet {
	return &packets.Packet{SenderId: sender, Msg: packets.NewPlayer(sender, &objects.Player{X: x, Y: y, Radius: 25})}
}

func TestNearbyPlayersGetEveryUpdateFarOnesOneInTen(t *testing.T) {
	h := testHub()
	near := addPlayer(h, 2, 300, 0, 25)
	far := addPlayer(h, 3, 5000, 0, 25)
	addPlayer(h, 1, 0, 0, 25) // the sender

	for i := 0; i < 20; i++ {
		h.broadcast(positionUpdate(1, 0, 0))
	}

	if got := len(near.received); got != 20 {
		t.Errorf("near player got %d updates, want 20", got)
	}
	if got := len(far.received); got != 2 {
		t.Errorf("far player got %d updates, want 2 (one in %d)", got, farUpdateEvery)
	}
}

func TestBigPlayersSeeFurther(t *testing.T) {
	h := testHub()
	small := addPlayer(h, 2, 2500, 0, 25)  // reach 1000: can't see a player 2500 away
	big := addPlayer(h, 3, -2500, 0, 200)  // reach 15*200 = 3000: can
	addPlayer(h, 1, 0, 0, 25)

	for i := 0; i < 10; i++ {
		h.broadcast(positionUpdate(1, 0, 0))
	}
	if len(small.received) != 1 || len(big.received) != 10 {
		t.Errorf("small got %d (want 1), big got %d (want 10)", len(small.received), len(big.received))
	}
}

func TestOtherMessagesAlwaysReachEveryone(t *testing.T) {
	h := testHub()
	far := addPlayer(h, 2, 9000, 9000, 25)
	addPlayer(h, 1, 0, 0, 25)

	h.broadcast(&packets.Packet{SenderId: 1, Msg: packets.NewChat("hi")})
	if len(far.received) != 1 {
		t.Fatalf("chat should reach far players, got %d messages", len(far.received))
	}
}

func TestPlayersNotInGameSeeEverything(t *testing.T) {
	h := testHub()
	lobby := &fakeClient{id: 2}
	h.Clients.Add(lobby, 2) // connected, but no player yet
	addPlayer(h, 1, 0, 0, 25)

	for i := 0; i < 5; i++ {
		h.broadcast(positionUpdate(1, 0, 0))
	}
	if len(lobby.received) != 5 {
		t.Fatalf("got %d, want 5", len(lobby.received))
	}
}
