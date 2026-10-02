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
