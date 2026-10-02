package objects

import "time"

type Player struct {
	Name      string
	X         float64
	Y         float64
	Radius    float64
	Direction float64
	Speed     float64
	BestScore int64
	DbId      int64
	Color     int32
}

type Spore struct {
	X         float64
	Y         float64
	Radius    float64
	DroppedBy *Player
	DroppedAt time.Time

	// Thrown with W or burst out of a blob by a virus. Clients animate these flying
	// out from (FromX, FromY), and OwnerId's blob can't eat them for LockFor.
	Ejected      bool
	FromX, FromY float64
	OwnerId      uint64
	LockFor      time.Duration
}

// Virus is a green spiky blob. Anything big enough to cover it bursts, losing
// mass as spores. Feeding one (W) makes it grow, and enough feeds make it shoot
// out a new virus, which bursts whatever big blob it runs into.
type Virus struct {
	X      float64
	Y      float64
	Radius float64
	VX, VY float64 // only a freshly shot virus moves
	Feeds  int
}
