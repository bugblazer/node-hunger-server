package objects

import (
	"sync"
	"testing"
)

// The original bug: IDs came from len(map), so two players joining at the same
// moment (or one joining after another left) could get the same ID.
func TestAddGivesUniqueIdsUnderConcurrency(t *testing.T) {
	c := NewSharedCollection[int]()
	const n = 500
	ids := make(chan uint64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids <- c.Add(i)
		}(i)
	}
	wg.Wait()
	close(ids)

	seen := map[uint64]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %d", id)
		}
		seen[id] = true
	}
	if c.Len() != n {
		t.Fatalf("len = %d, want %d", c.Len(), n)
	}
}

func TestIdsAreNotReusedAfterRemove(t *testing.T) {
	c := NewSharedCollection[string]()
	a := c.Add("a")
	c.Remove(a)
	if b := c.Add("b"); b == a {
		t.Fatalf("id %d was reused after removal", a)
	}
}

func TestClampToMapKeepsTheWholeCircleInside(t *testing.T) {
	x, y := ClampToMap(5000, -4000, 50)
	if x != MapHalfSize-50 || y != -(MapHalfSize-50) {
		t.Fatalf("got (%v, %v)", x, y)
	}
	if x, y := ClampToMap(10, -20, 50); x != 10 || y != -20 {
		t.Fatalf("points inside the map must not move, got (%v, %v)", x, y)
	}
}

func TestSpawnsStayInsideTheMapEvenWhenCrowded(t *testing.T) {
	spores := NewSharedCollection[*Spore]()
	// A crowded map: the old code doubled its search area and spawned outside it.
	for i := 0; i < 3000; i++ {
		x, y := SpawnCoords(10, nil, spores)
		if x < -MapHalfSize+10 || x > MapHalfSize-10 || y < -MapHalfSize+10 || y > MapHalfSize-10 {
			t.Fatalf("spawned outside the map at (%v, %v)", x, y)
		}
		spores.Add(&Spore{X: x, Y: y, Radius: 10})
	}
}
