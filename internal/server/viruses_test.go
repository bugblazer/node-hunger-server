package server

import (
	"testing"

	"server/internal/server/objects"
	"server/pkg/packets"
)

func virusTestHub() *Hub {
	h := testHub()
	h.BroadcastChan = make(chan *packets.Packet, 64)
	h.SharedGameObjects.Viruses = objects.NewSharedCollection[*objects.Virus]()
	return h
}

func TestVirusBurstsOnlyBlobsBigEnoughToCoverIt(t *testing.T) {
	h := virusTestHub()
	virusId := h.SharedGameObjects.Viruses.Add(&objects.Virus{X: 0, Y: 0, Radius: VirusRadius})
	addPlayer(h, 1, 10, 0, VirusRadius) // same size: hides underneath, no burst

	h.burstPlayersOnViruses()
	if _, ok := h.SharedGameObjects.Viruses.Get(virusId); !ok {
		t.Fatal("a blob no bigger than the virus burst it")
	}

	addPlayer(h, 2, 30, 0, VirusRadius*1.5) // big, and the virus centre is under it
	h.burstPlayersOnViruses()
	if _, ok := h.SharedGameObjects.Viruses.Get(virusId); ok {
		t.Fatal("a big blob covering the virus didn't burst on it")
	}
	msg := (<-h.BroadcastChan).Msg.(*packets.Packet_VirusConsumed).VirusConsumed
	if msg.VirusId != virusId || msg.PlayerId != 2 {
		t.Fatalf("got virus %d / player %d, want %d / 2", msg.VirusId, msg.PlayerId, virusId)
	}
}

func TestVirusShootsAfterEnoughFeeds(t *testing.T) {
	h := virusTestHub()
	virusId := h.SharedGameObjects.Viruses.Add(&objects.Virus{Radius: VirusRadius})

	for range VirusFeedsToShoot - 1 {
		h.feedVirus(VirusFeed{VirusId: virusId, Mass: 300, DirX: 1})
	}
	v, _ := h.SharedGameObjects.Viruses.Get(virusId)
	if v.Radius <= VirusRadius || h.SharedGameObjects.Viruses.Len() != 1 {
		t.Fatalf("after %d feeds: radius %.1f, %d viruses; want it grown and not shot yet",
			VirusFeedsToShoot-1, v.Radius, h.SharedGameObjects.Viruses.Len())
	}

	h.feedVirus(VirusFeed{VirusId: virusId, Mass: 300, DirX: 1})
	if v.Radius != VirusRadius || h.SharedGameObjects.Viruses.Len() != 2 {
		t.Fatalf("on the last feed: radius %.1f, %d viruses; want it reset and a new one shot", v.Radius, h.SharedGameObjects.Viruses.Len())
	}
	h.SharedGameObjects.Viruses.ForEach(func(id uint64, shot *objects.Virus) {
		if id != virusId && shot.VX <= 0 {
			t.Fatalf("shot virus moving at %.0f, want it heading the way it was fed (+x)", shot.VX)
		}
	})
}

func TestShotVirusSlowsToAStopInsideTheMap(t *testing.T) {
	h := virusTestHub()
	v := &objects.Virus{X: objects.MapHalfSize - 200, Radius: VirusRadius, VX: virusShotSpeed}
	h.SharedGameObjects.Viruses.Add(v)

	for range 200 {
		h.moveViruses(virusTick.Seconds())
		for len(h.BroadcastChan) > 0 {
			<-h.BroadcastChan
		}
	}
	if v.VX != 0 || v.VY != 0 {
		t.Fatalf("still moving after 10 s: %.1f, %.1f", v.VX, v.VY)
	}
	if v.X+v.Radius > objects.MapHalfSize {
		t.Fatalf("went through the wall: x = %.1f", v.X)
	}
}
