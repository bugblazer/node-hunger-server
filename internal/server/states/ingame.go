package states

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"server/internal/server"
	"server/internal/server/db"
	"server/internal/server/objects"
	"server/pkg/packets"
	"time"
)

type InGame struct {
	client                 server.ClientInterfacer
	player                 *objects.Player
	logger                 *log.Logger
	cancelPlayerUpdateLoop context.CancelFunc
	lastFeed               time.Time
}

// W (feed) throws a small spore ahead of the blob, Agar.io style. Holding W
// repeats; the server allows one throw per feedCooldown.
const (
	feedCooldown    = 90 * time.Millisecond
	feedMinRadius   = 35.0  // too small to feed below this (a new blob is 20)
	feedSporeRadius = 11.0  // what each throw costs and what it's worth to whoever eats it
	feedThrowDist   = 240.0 // how far past the blob's edge it lands
	feedOwnerLockup = time.Second
)

// A virus burst sheds this share of the blob's mass as spores around it.
const (
	burstMassShare   = 0.4
	burstSporeMass   = 400.0 // aim for spores about this heavy...
	burstMinSpores   = 8     // ...but always at least this many
	burstMaxSpores   = 24    // and never more than this
	burstSpreadMin   = 25.0  // how far past the blob's new edge they land
	burstSpreadMax   = 220.0
	burstOwnerLockup = 2 * time.Second // before the burst blob can eat its own spores back
)

func (g *InGame) Name() string {
	return "InGame"
}

func (g *InGame) SetClient(client server.ClientInterfacer) {
	g.client = client
	loggingPrefix := fmt.Sprintf("Client %d [%s]: ", client.Id(), g.Name())
	g.logger = log.New(log.Writer(), loggingPrefix, log.LstdFlags)
}

func (g *InGame) OnEnter() {
	log.Printf("Adding player %s to the shared collection", g.player.Name)
	go g.client.SharedGameObjects().Players.Add(g.player, g.client.Id())

	// Set the initial properties of the player
	// Radius first: spawning needs the real size to keep the player clear of others and the walls.
	g.player.Speed = 150.0
	g.player.Radius = 20.0
	g.player.X, g.player.Y = objects.SpawnCoords(g.player.Radius, g.client.SharedGameObjects().Players, nil)

	// Send the player's initial state to the client
	g.client.SocketSend(packets.NewPlayer(g.client.Id(), g.player))

	// Send the spores to the client in the background
	go g.sendInitialSpores(20, 50*time.Millisecond)

	g.sendInitialViruses()
}

func (g *InGame) HandleMessage(senderId uint64, message packets.Msg) {
	switch message := message.(type) {
	case *packets.Packet_Player:
		g.handlePlayer(senderId, message)
	case *packets.Packet_PlayerDirection:
		g.handlePlayerDirection(senderId, message)
	case *packets.Packet_Chat:
		g.handleChat(senderId, message)
	case *packets.Packet_SporeConsumed:
		g.handleSporeConsumed(senderId, message)
	case *packets.Packet_PlayerConsumed:
		g.handlePlayerConsumed(senderId, message)
	case *packets.Packet_Spore:
		g.handleSpore(senderId, message)
	case *packets.Packet_SporesBatch:
		g.client.SocketSendAs(message, senderId)
	case *packets.Packet_Disconnect:
		g.handleDisconnect(senderId, message)
	case *packets.Packet_Feed:
		g.handleFeed(senderId, message)
	case *packets.Packet_Virus, *packets.Packet_VirusesBatch:
		g.client.SocketSendAs(message, senderId)
	case *packets.Packet_VirusConsumed:
		g.handleVirusConsumed(senderId, message)
	}
}

func (g *InGame) OnExit() {
	if g.cancelPlayerUpdateLoop != nil {
		g.cancelPlayerUpdateLoop()
	}
	g.client.SharedGameObjects().Players.Remove(g.client.Id())
	g.syncPlayerBestScore()
}

func (g *InGame) handlePlayer(senderId uint64, message *packets.Packet_Player) {
	if senderId == g.client.Id() {
		g.logger.Println("Received player message from our own client, ignoring")
		return
	}

	g.client.SocketSendAs(message, senderId)
}

func (g *InGame) handlePlayerDirection(senderId uint64, message *packets.Packet_PlayerDirection) {
	if senderId != g.client.Id() {
		g.logger.Println("Received player direction message from a different client, ignoring")
		return
	}

	g.player.Direction = message.PlayerDirection.Direction

	// If this is the first time receiving a player direction message from our client, start the player update loop
	if g.cancelPlayerUpdateLoop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		g.cancelPlayerUpdateLoop = cancel
		go g.playerUpdateLoop(ctx)
	}
}

func (g *InGame) handleChat(senderId uint64, message *packets.Packet_Chat) {
	if senderId == g.client.Id() {
		g.client.Broadcast(message)
	} else {
		g.client.SocketSendAs(message, senderId)
	}
}

func (g *InGame) handleSporeConsumed(senderId uint64, message *packets.Packet_SporeConsumed) {
	if senderId != g.client.Id() {
		g.client.SocketSendAs(message, senderId)
		return
	}

	// If the spore was supposedly consumed by our player, we need to verify the plausibility of the event
	errMsg := "Could not verify spore consumption: "

	// First, check if the spore exists
	sporeId := message.SporeConsumed.SporeId
	spore, err := g.getSpore(sporeId)
	if err != nil {
		g.logger.Println(errMsg + err.Error())
		return
	}

	// Next, check if the spore is closed enough to be consumed
	err = g.validatePlayerCloseToObject(spore.X, spore.Y, spore.Radius, 10)
	if err != nil {
		g.logger.Println(errMsg + err.Error())
		return
	}

	// Finally, check if the spore wasn't dropped by the player too recently
	err = g.validatePlayerDropCooldown(spore, 10)
	if err != nil {
		g.logger.Println(errMsg + err.Error())
		return
	}

	// If we made this far, the spore consumption is valid, so grow the player, remove the spore, and broadcast the event
	sporeMass := radToMass(spore.Radius)
	g.player.Radius = g.nextRadius(sporeMass)

	go g.client.SharedGameObjects().Spores.Remove(sporeId)

	g.client.Broadcast(message)

	go g.syncPlayerBestScore()
}

func (g *InGame) handlePlayerConsumed(senderId uint64, message *packets.Packet_PlayerConsumed) {
	if senderId != g.client.Id() {
		g.client.SocketSendAs(message, senderId)

		if message.PlayerConsumed.PlayerId == g.client.Id() {
			g.logger.Println("Player was consumed, respawning")
			g.client.SetState(&InGame{player: respawnedPlayer(g.player)})
		}

		return
	}

	// If the other player was supposedly consumed by our player, we need to verify the plausibility of the event
	errMsg := "Could not verify player consumption: "

	// First check if the player exists
	otherId := message.PlayerConsumed.PlayerId
	other, err := g.getOtherPlayer(otherId)
	if err != nil {
		g.logger.Println(errMsg + err.Error())
		return
	}

	// Next, check if the other player is closed enough to be consumed
	err = g.validatePlayerCloseToObject(other.X, other.Y, other.Radius, 10)
	if err != nil {
		g.logger.Println(errMsg + err.Error())
		return
	}

	// Finally, check the other player's mass is less than our player's
	ourMass := radToMass(g.player.Radius)
	otherMass := radToMass(other.Radius)
	if ourMass <= otherMass*1.5 {
		g.logger.Printf(errMsg+"player not massive enough to consume the other player (our radius: %f, other radius: %f)", g.player.Radius, other.Radius)
		return
	}

	// If we made it this far, the player consumption is valid, so grow the player, remove the consumed other, and broadcast the event
	g.player.Radius = g.nextRadius(otherMass)

	go g.client.SharedGameObjects().Players.Remove(otherId)

	g.client.Broadcast(message)

	go g.syncPlayerBestScore()
}

func (g *InGame) handleSpore(senderId uint64, message *packets.Packet_Spore) {
	g.client.SocketSendAs(message, senderId)
}

func (g *InGame) handleDisconnect(senderId uint64, message *packets.Packet_Disconnect) {
	if senderId == g.client.Id() {
		g.client.Broadcast(message)
		g.client.SetState(&Connected{})
	} else {
		go g.client.SocketSendAs(message, senderId)
	}
}

// handleFeed throws a spore in the direction the player aimed. If a virus is in
// the way, the virus takes the mass instead (see server.VirusFeed).
func (g *InGame) handleFeed(senderId uint64, message *packets.Packet_Feed) {
	if senderId != g.client.Id() {
		return
	}
	if time.Since(g.lastFeed) < feedCooldown || g.player.Radius < feedMinRadius {
		return
	}
	g.lastFeed = time.Now()

	sporeMass := radToMass(feedSporeRadius)
	g.player.Radius = g.nextRadius(-sporeMass)

	dirX, dirY := math.Cos(message.Feed.Direction), math.Sin(message.Feed.Direction)
	// Start just outside the blob so it doesn't land back inside it.
	edge := g.player.Radius + feedSporeRadius + 2
	fromX, fromY := g.player.X+dirX*edge, g.player.Y+dirY*edge
	toX, toY := objects.ClampToMap(fromX+dirX*feedThrowDist, fromY+dirY*feedThrowDist, feedSporeRadius)

	shared := g.client.SharedGameObjects()
	virusId, virusT, hitVirus := virusOnPath(shared.Viruses, fromX, fromY, toX, toY, feedSporeRadius)
	_, playerT, catchX, catchY, hitPlayer := playerOnPath(shared.Players, g.client.Id(), fromX, fromY, toX, toY, feedSporeRadius)
	if hitVirus && (!hitPlayer || virusT <= playerT) {
		shared.FeedVirus(server.VirusFeed{VirusId: virusId, Mass: sporeMass, DirX: dirX, DirY: dirY})
		return
	}
	// A blob in the way catches the throw: it lands inside that blob instead of flying
	// through it (nobody can eat a spore mid-air), so it gets eaten when it lands.
	if hitPlayer {
		toX, toY = catchX, catchY
	}

	spore := &objects.Spore{
		X:         toX,
		Y:         toY,
		Radius:    feedSporeRadius,
		DroppedBy: g.player,
		DroppedAt: time.Now(),
		Ejected:   true,
		FromX:     fromX,
		FromY:     fromY,
		OwnerId:   g.client.Id(),
		// Thrown into a wall it can land right next to the blob; don't let the
		// thrower take it straight back.
		LockFor: feedOwnerLockup,
	}
	sporeId := shared.Spores.Add(spore)
	g.client.Broadcast(packets.NewSpore(sporeId, spore))
	g.client.SocketSend(packets.NewSpore(sporeId, spore))
}

// closestOnPath returns how far along a throw from (fromX, fromY) to (toX, toY)
// it passes closest to (x, y), from 0 to 1, and how far away it is then (squared).
func closestOnPath(fromX, fromY, toX, toY, x, y float64) (t, distSq float64) {
	segX, segY := toX-fromX, toY-fromY
	if segLenSq := segX*segX + segY*segY; segLenSq > 0 {
		t = min(max(((x-fromX)*segX+(y-fromY)*segY)/segLenSq, 0), 1)
	}
	dx, dy := fromX+segX*t-x, fromY+segY*t-y
	return t, dx*dx + dy*dy
}

// virusOnPath finds the virus a throw runs into first, if any, and how far along
// the throw that happens.
func virusOnPath(viruses *objects.SharedCollection[*objects.Virus], fromX, fromY, toX, toY, radius float64) (uint64, float64, bool) {
	bestId, bestT, found := uint64(0), math.Inf(1), false
	viruses.ForEach(func(id uint64, v *objects.Virus) {
		t, distSq := closestOnPath(fromX, fromY, toX, toY, v.X, v.Y)
		reach := v.Radius + radius
		if distSq <= reach*reach && t < bestT {
			bestId, bestT, found = id, t, true
		}
	})
	return bestId, bestT, found
}

func firstVirusOnPath(viruses *objects.SharedCollection[*objects.Virus], fromX, fromY, toX, toY, radius float64) (uint64, bool) {
	id, _, found := virusOnPath(viruses, fromX, fromY, toX, toY, radius)
	return id, found
}

// playerOnPath finds the first blob (other than the thrower) a throw passes
// over, and a point inside that blob for the spore to land on.
func playerOnPath(players *objects.SharedCollection[*objects.Player], throwerId uint64, fromX, fromY, toX, toY, radius float64) (uint64, float64, float64, float64, bool) {
	bestId, bestT, found := uint64(0), math.Inf(1), false
	var landX, landY float64
	players.ForEach(func(id uint64, p *objects.Player) {
		if id == throwerId {
			return
		}
		t, distSq := closestOnPath(fromX, fromY, toX, toY, p.X, p.Y)
		reach := p.Radius + radius
		if distSq > reach*reach || t >= bestT {
			return
		}
		bestId, bestT, found = id, t, true
		// The closest point on the throw, pulled in to half the blob's radius from its
		// centre so the spore is well inside it when it lands.
		cx, cy := fromX+(toX-fromX)*t, fromY+(toY-fromY)*t
		dx, dy := cx-p.X, cy-p.Y
		if d := math.Sqrt(distSq); d > p.Radius/2 {
			dx, dy = dx/d*p.Radius/2, dy/d*p.Radius/2
		}
		landX, landY = p.X+dx, p.Y+dy
	})
	return bestId, bestT, landX, landY, found
}

func (g *InGame) handleVirusConsumed(senderId uint64, message *packets.Packet_VirusConsumed) {
	g.client.SocketSendAs(message, senderId)
	if message.VirusConsumed.PlayerId == g.client.Id() {
		g.burst()
	}
}

// burst is what a virus does to a blob: a share of its mass flies off as spores
// in a ring around it, free for anyone nearby to eat. The blob itself has to
// wait burstOwnerLockup before it can win them back.
func (g *InGame) burst() {
	mass := radToMass(g.player.Radius)
	shed := mass * burstMassShare
	count := int(min(max(math.Round(shed/burstSporeMass), burstMinSpores), burstMaxSpores))
	sporeRadius := massToRad(shed / float64(count))

	g.player.Radius = massToRad(mass - shed)
	g.logger.Printf("Burst by a virus: shedding %.0f mass as %d spores", shed, count)

	shared := g.client.SharedGameObjects()
	batch := make(map[uint64]*objects.Spore, count)
	start := rand.Float64() * 2 * math.Pi
	for i := range count {
		angle := start + 2*math.Pi*float64(i)/float64(count) + (rand.Float64()-0.5)*0.4
		dist := g.player.Radius + sporeRadius + burstSpreadMin + rand.Float64()*(burstSpreadMax-burstSpreadMin)
		x, y := objects.ClampToMap(g.player.X+math.Cos(angle)*dist, g.player.Y+math.Sin(angle)*dist, sporeRadius)
		spore := &objects.Spore{
			X:         x,
			Y:         y,
			Radius:    sporeRadius,
			DroppedBy: g.player,
			DroppedAt: time.Now(),
			Ejected:   true,
			FromX:     g.player.X,
			FromY:     g.player.Y,
			OwnerId:   g.client.Id(),
			LockFor:   burstOwnerLockup,
		}
		batch[shared.Spores.Add(spore)] = spore
	}

	g.client.Broadcast(packets.NewSporesBatch(batch))
	g.client.SocketSend(packets.NewSporesBatch(batch))
	// The new size goes out with the next position update (20 a second), but a blob
	// that isn't moving yet has no update loop, so send it now too.
	update := packets.NewPlayer(g.client.Id(), g.player)
	g.client.Broadcast(update)
	g.client.SocketSend(update)
}

func (g *InGame) sendInitialViruses() {
	viruses := make(map[uint64]*objects.Virus)
	g.client.SharedGameObjects().Viruses.ForEach(func(id uint64, v *objects.Virus) {
		viruses[id] = v
	})
	g.client.SocketSend(packets.NewVirusesBatch(viruses))
}

func (g *InGame) playerUpdateLoop(ctx context.Context) {
	const delta float64 = 0.05
	ticker := time.NewTicker(time.Duration(delta*1000) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			g.syncPlayer(delta)
		case <-ctx.Done():
			return
		}
	}
}

func (g *InGame) syncPlayer(delta float64) {
	newX := g.player.X + g.player.Speed*math.Cos(g.player.Direction)*delta
	newY := g.player.Y + g.player.Speed*math.Sin(g.player.Direction)*delta

	// The map has walls: a player who steers into one slides along it.
	g.player.X, g.player.Y = objects.ClampToMap(newX, newY, g.player.Radius)

	// Drop a spore
	probability := g.player.Radius / float64(server.MaxSpores*5)
	if rand.Float64() < probability && g.player.Radius > 10 {
		spore := &objects.Spore{
			X:         g.player.X,
			Y:         g.player.Y,
			Radius:    min(5+g.player.Radius/50, 15),
			DroppedBy: g.player,
			DroppedAt: time.Now(),
		}
		sporeId := g.client.SharedGameObjects().Spores.Add(spore)
		g.client.Broadcast(packets.NewSpore(sporeId, spore))
		go g.client.SocketSend(packets.NewSpore(sporeId, spore))
		g.player.Radius = g.nextRadius(-radToMass(spore.Radius))
	}

	// Broadcast the updated player state
	updatePlayer := packets.NewPlayer(g.client.Id(), g.player)
	g.client.Broadcast(updatePlayer)
	go g.client.SocketSend(updatePlayer)
}

func (g *InGame) sendInitialSpores(batchSize int, delay time.Duration) {
	sporesBatch := make(map[uint64]*objects.Spore, batchSize)

	g.client.SharedGameObjects().Spores.ForEach(func(sporeId uint64, spore *objects.Spore) {
		sporesBatch[sporeId] = spore

		if len(sporesBatch) >= batchSize {
			g.client.SocketSend(packets.NewSporesBatch(sporesBatch))
			sporesBatch = make(map[uint64]*objects.Spore, batchSize)
			time.Sleep(delay)
		}
	})

	// Send any remaining spores
	if len(sporesBatch) > 0 {
		g.client.SocketSend(packets.NewSporesBatch(sporesBatch))
	}
}

func (g *InGame) getSpore(sporeId uint64) (*objects.Spore, error) {
	spore, exists := g.client.SharedGameObjects().Spores.Get(sporeId)
	if !exists {
		return nil, fmt.Errorf("spore with ID %d does not exist", sporeId)
	}
	return spore, nil
}

func (g *InGame) getOtherPlayer(playerId uint64) (*objects.Player, error) {
	player, exists := g.client.SharedGameObjects().Players.Get(playerId)
	if !exists {
		return nil, fmt.Errorf("player with ID %d does not exist", playerId)
	}
	return player, nil
}

func (g *InGame) validatePlayerCloseToObject(objX, objY, objRadius, buffer float64) error {
	realDX := g.player.X - objX
	realDY := g.player.Y - objY
	realDistSq := realDX*realDX + realDY*realDY

	thresholdDist := g.player.Radius + buffer + objRadius
	thresholdDistSq := thresholdDist * thresholdDist

	if realDistSq > thresholdDistSq {
		return fmt.Errorf("player is too far from the object (distSq: %f, thresholdSq: %f)", realDistSq, thresholdDistSq)
	}
	return nil
}

func (g *InGame) validatePlayerDropCooldown(spore *objects.Spore, buffer float64) error {
	minAcceptableDistance := spore.Radius + g.player.Radius + buffer
	minAcceptableTime := time.Duration(minAcceptableDistance/g.player.Speed*1000) * time.Millisecond
	minAcceptableTime = max(minAcceptableTime, spore.LockFor)
	if spore.DroppedBy == g.player && time.Since(spore.DroppedAt) < minAcceptableTime {
		return fmt.Errorf("player dropped the spore too recently (time: %v, min acceptable time: %v)", time.Since(spore.DroppedAt), minAcceptableTime)
	}
	return nil
}

func radToMass(radius float64) float64 {
	return math.Pi * radius * radius
}

func massToRad(mass float64) float64 {
	return math.Sqrt(mass / math.Pi)
}

func (g *InGame) nextRadius(massDiff float64) float64 {
	oldMass := radToMass(g.player.Radius)
	newMass := oldMass + massDiff
	return massToRad(newMass)
}

func (g *InGame) syncPlayerBestScore() {
	currentScore := int64(math.Round(radToMass(g.player.Radius)))
	if currentScore > g.player.BestScore {
		g.player.BestScore = currentScore
		err := g.client.DbTx().Queries.UpdatePlayerBestScore(g.client.DbTx().Ctx, db.UpdatePlayerBestScoreParams{
			ID:        g.player.DbId,
			BestScore: g.player.BestScore,
		})
		if err != nil {
			g.logger.Printf("Error updating player best score: %v", err)
		}
	}
}

// respawnedPlayer is the fresh blob a player gets after being eaten: new position
// and size (set in OnEnter), same identity. The respawn used to copy only the name,
// so the colour came through as 0 (fully transparent: other players saw just a
// floating name) and the database id was lost, so best scores stopped saving.
func respawnedPlayer(p *objects.Player) *objects.Player {
	return &objects.Player{
		Name:      p.Name,
		DbId:      p.DbId,
		BestScore: p.BestScore,
		Color:     p.Color,
	}
}
