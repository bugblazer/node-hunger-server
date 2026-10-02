package states

import (
	"testing"

	"server/internal/server/objects"
)

func rgba(r, g, b uint32) int32 { return int32(r<<24 | g<<16 | b<<8 | 0xff) }

func TestOnlyBrightColoursAreAllowed(t *testing.T) {
	allowed := map[string]int32{
		"red":         rgba(255, 0, 0),
		"pastel blue": rgba(140, 180, 255), // saturation 0.45, the palest the wheel offers
		"yellow":      rgba(255, 230, 40),
	}
	blocked := map[string]int32{
		"map grey":   rgba(42, 42, 42),
		"black":      rgba(0, 0, 0),
		"white":      rgba(255, 255, 255),
		"dark red":   rgba(120, 0, 0),
		"light grey": rgba(200, 200, 200),
	}
	for name, c := range allowed {
		if !isVisibleColor(c) {
			t.Errorf("%s should be allowed", name)
		}
	}
	for name, c := range blocked {
		if isVisibleColor(c) {
			t.Errorf("%s should be blocked", name)
		}
	}
}

func TestRespawnKeepsColourAndAccount(t *testing.T) {
	eaten := &objects.Player{Name: "idare", DbId: 7, BestScore: 4462, Color: rgba(255, 140, 0), X: 120, Y: -40, Radius: 90}
	fresh := respawnedPlayer(eaten)
	if fresh.Color != eaten.Color || fresh.DbId != 7 || fresh.BestScore != 4462 || fresh.Name != "idare" {
		t.Fatalf("respawn lost identity: %+v", fresh)
	}
	if fresh.Radius != 0 || fresh.X != 0 {
		t.Fatalf("respawn should start fresh (position and size are set on enter): %+v", fresh)
	}
}

func TestFirstVirusOnPathPicksTheNearestVirusInTheWay(t *testing.T) {
	viruses := objects.NewSharedCollection[*objects.Virus]()
	far := viruses.Add(&objects.Virus{X: 200, Y: 0, Radius: 50})
	near := viruses.Add(&objects.Virus{X: 100, Y: 30, Radius: 50})
	viruses.Add(&objects.Virus{X: 100, Y: 400, Radius: 50}) // off to the side

	if id, hit := firstVirusOnPath(viruses, 0, 0, 300, 0, 10); !hit || id != near {
		t.Fatalf("got virus %d (hit %v), want the nearer one %d (not %d)", id, hit, near, far)
	}
	if _, hit := firstVirusOnPath(viruses, 0, 0, -300, 0, 10); hit {
		t.Fatal("a throw away from every virus hit one")
	}
}
