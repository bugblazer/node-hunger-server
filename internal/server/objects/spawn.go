package objects

import "math/rand/v2"

var getPlayerPosition = func(p *Player) (float64, float64) { return p.X, p.Y }
var getPlayerRadius = func(p *Player) float64 { return p.Radius }
var getSporePosition = func(s *Spore) (float64, float64) { return s.X, s.Y }
var getSporeRadius = func(s *Spore) float64 { return s.Radius }

func isTooClose[T any](x float64, y float64, radius float64, objects *SharedCollection[T], getPosition func(T) (float64, float64), getRadius func(T) float64) bool {
	// Not too close if there are no objects
	if objects == nil {
		return false
	}

	// Check if any object is too close
	tooClose := false
	objects.ForEach(func(_ uint64, object T) {
		if tooClose {
			return
		}

		objX, objY := getPosition(object)
		objRad := getRadius(object)
		xDst := objX - x
		yDst := objY - y
		dstSq := xDst*xDst + yDst*yDst

		if dstSq <= (radius+objRad)*(radius+objRad) {
			tooClose = true
			return
		}
	})

	return tooClose
}

// MapHalfSize is half the width of the square map: it spans -MapHalfSize..MapHalfSize
// on both axes. Players can't leave it, and nothing spawns outside it. The web client
// draws the walls at the same place (MAP_HALF_SIZE in objects/map_border).
const MapHalfSize = 3000.0

// ClampToMap keeps a circle of the given radius fully inside the map.
func ClampToMap(x, y, radius float64) (float64, float64) {
	limit := max(MapHalfSize-radius, 0)
	return min(max(x, -limit), limit), min(max(y, -limit), limit)
}

// SpawnCoords picks a random point inside the map that doesn't overlap the given
// players or spores. The old version doubled its search area whenever the map got
// crowded, which spawned spores (and players) outside the playable map; now it
// stays inside and, if it can't find a free spot, accepts a slightly crowded one.
func SpawnCoords(radius float64, playersToAvoid *SharedCollection[*Player], sporesToAvoid *SharedCollection[*Spore]) (float64, float64) {
	const maxTries = 25
	limit := max(MapHalfSize-radius, 0)

	var x, y float64
	for range maxTries {
		x = limit * (2*rand.Float64() - 1)
		y = limit * (2*rand.Float64() - 1)

		if !isTooClose(x, y, radius, playersToAvoid, getPlayerPosition, getPlayerRadius) &&
			!isTooClose(x, y, radius, sporesToAvoid, getSporePosition, getSporeRadius) {
			return x, y
		}
	}
	return x, y
}
