package server

import (
	"log"
	"math"
	"server/internal/server/objects"
	"server/pkg/packets"
	"time"
)

// Viruses work like Agar.io's. A blob big enough to cover one bursts: it loses a
// share of its mass as spores scattered around it (see InGame.burst). Feeding a
// virus with W grows it, and every VirusFeedsToShoot feeds it shoots a new virus
// the way it was fed, which bursts the first big blob in its path. Small blobs
// can hide underneath a virus safely.
const (
	MaxViruses        = 20    // viruses the map is topped back up to
	maxVirusesTotal   = 35    // shot viruses can go above MaxViruses, up to this
	VirusRadius       = 50.0  // a new virus; a fresh player is 20
	VirusPopRatio     = 1.15  // a blob bursts on a virus if its radius is this much bigger
	VirusFeedsToShoot = 7     // feeds before a virus shoots a new one
	virusShotSpeed    = 1100. // world units/second when shot; it slows to a stop after ~550
	virusFriction     = 0.9   // speed kept per tick
	virusTick         = 50 * time.Millisecond
	virusReplenish    = 3 * time.Second
)

// VirusFeed is a W throw that hit a virus. Throws happen on players' goroutines;
// the virus loop applies them, so only that one goroutine ever changes a virus.
type VirusFeed struct {
	VirusId    uint64
	Mass       float64
	DirX, DirY float64 // unit vector the mass was thrown along
}

// FeedVirus queues a throw for the virus loop. It never blocks a player's
// goroutine: if the queue is somehow full, the throw is dropped.
func (s *SharedGameObjects) FeedVirus(feed VirusFeed) {
	select {
	case s.VirusFeeds <- feed:
	default:
		log.Printf("Virus feed queue full, dropping a feed for virus %d", feed.VirusId)
	}
}

func (h *Hub) newVirus() *objects.Virus {
	x, y := objects.SpawnCoords(VirusRadius, h.SharedGameObjects.Players, nil)
	return &objects.Virus{X: x, Y: y, Radius: VirusRadius}
}

func (h *Hub) placeViruses() {
	for range MaxViruses {
		h.SharedGameObjects.Viruses.Add(h.newVirus())
	}
}

// virusLoop owns every virus: it moves shot ones, bursts blobs that cover one,
// applies feeds, and tops the map back up.
func (h *Hub) virusLoop() {
	ticker := time.NewTicker(virusTick)
	defer ticker.Stop()
	lastReplenish := time.Now()

	for {
		select {
		case feed := <-h.SharedGameObjects.VirusFeeds:
			h.feedVirus(feed)
		case <-ticker.C:
			h.moveViruses(virusTick.Seconds())
			h.burstPlayersOnViruses()
			if time.Since(lastReplenish) >= virusReplenish {
				lastReplenish = time.Now()
				h.replenishVirus()
			}
		}
	}
}

func (h *Hub) broadcastFromServer(msg packets.Msg) {
	h.BroadcastChan <- &packets.Packet{SenderId: 0, Msg: msg}
}

func (h *Hub) moveViruses(dt float64) {
	h.SharedGameObjects.Viruses.ForEach(func(id uint64, v *objects.Virus) {
		if v.VX == 0 && v.VY == 0 {
			return
		}
		x, y := v.X+v.VX*dt, v.Y+v.VY*dt
		cx, cy := objects.ClampToMap(x, y, v.Radius)
		// Bounce off the walls instead of sticking to them.
		if cx != x {
			v.VX = -v.VX
		}
		if cy != y {
			v.VY = -v.VY
		}
		v.X, v.Y = cx, cy

		v.VX *= virusFriction
		v.VY *= virusFriction
		if math.Hypot(v.VX, v.VY) < 20 {
			v.VX, v.VY = 0, 0
		}
		h.broadcastFromServer(packets.NewVirus(id, v))
	})
}

// burstPlayersOnViruses checks every virus against every blob. A blob bursts when
// it's big enough and the virus's centre is underneath it. The virus is used up;
// the blob's own client works out the burst when the message reaches it.
func (h *Hub) burstPlayersOnViruses() {
	h.SharedGameObjects.Viruses.ForEach(func(virusId uint64, v *objects.Virus) {
		var victim uint64
		h.SharedGameObjects.Players.ForEach(func(playerId uint64, p *objects.Player) {
			if victim != 0 || p.Radius <= v.Radius*VirusPopRatio {
				return
			}
			dx, dy := p.X-v.X, p.Y-v.Y
			if dx*dx+dy*dy < p.Radius*p.Radius {
				victim = playerId
			}
		})
		if victim == 0 {
			return
		}
		h.SharedGameObjects.Viruses.Remove(virusId)
		h.broadcastFromServer(packets.NewVirusConsumed(virusId, victim))
	})
}

func (h *Hub) feedVirus(feed VirusFeed) {
	v, exists := h.SharedGameObjects.Viruses.Get(feed.VirusId)
	if !exists {
		return // burst someone on the way
	}

	v.Feeds++
	if v.Feeds < VirusFeedsToShoot {
		v.Radius = math.Sqrt((math.Pi*v.Radius*v.Radius + feed.Mass) / math.Pi)
		h.broadcastFromServer(packets.NewVirus(feed.VirusId, v))
		return
	}

	// Enough feeds: back to normal size, and shoot a new virus the way it was fed.
	v.Feeds = 0
	v.Radius = VirusRadius
	h.broadcastFromServer(packets.NewVirus(feed.VirusId, v))

	if h.SharedGameObjects.Viruses.Len() >= maxVirusesTotal {
		return
	}
	shot := &objects.Virus{
		X:      v.X + feed.DirX*VirusRadius,
		Y:      v.Y + feed.DirY*VirusRadius,
		Radius: VirusRadius,
		VX:     feed.DirX * virusShotSpeed,
		VY:     feed.DirY * virusShotSpeed,
	}
	shot.X, shot.Y = objects.ClampToMap(shot.X, shot.Y, shot.Radius)
	shotId := h.SharedGameObjects.Viruses.Add(shot)
	h.broadcastFromServer(packets.NewVirus(shotId, shot))
}

func (h *Hub) replenishVirus() {
	if h.SharedGameObjects.Viruses.Len() >= MaxViruses {
		return
	}
	v := h.newVirus()
	id := h.SharedGameObjects.Viruses.Add(v)
	h.broadcastFromServer(packets.NewVirus(id, v))
}
