// Load test: bots that register, log in, steer randomly and send timestamped chat.
// Usage: go run ./cmd/loadtest -url ws://localhost:8090/ws -n 30 -d 15s -p bot
// Reports total traffic, chat broadcast latency, and the gap between position updates
// from players near each bot (ideal: 50 ms, i.e. 20 updates a second).
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"server/pkg/packets"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

var (
	url      = flag.String("url", "ws://localhost:8090/ws", "server")
	n        = flag.Int("n", 10, "bots")
	dur      = flag.Duration("d", 20*time.Second, "duration")
	prefix   = flag.String("p", "bot", "username prefix")
	burst    = flag.Bool("burst", false, "register all bots at once (tests concurrent sign-ups) instead of one at a time")
	received atomic.Int64
	bytesIn  atomic.Int64
	inGame   atomic.Int64
	mu       sync.Mutex
	authMu   sync.Mutex // register/login one bot at a time (SQLite locks on concurrent writes)
	lat      []float64
	nearGaps []float64 // ms between consecutive updates from a nearby player
)

const nearDistance = 900.0 // inside every viewer's full-rate radius

func send(c *websocket.Conn, p *packets.Packet) error {
	b, _ := proto.Marshal(p)
	return c.WriteMessage(websocket.BinaryMessage, b)
}

func read(c *websocket.Conn) (*packets.Packet, error) {
	_, b, err := c.ReadMessage()
	if err != nil {
		return nil, err
	}
	bytesIn.Add(int64(len(b)))
	if n := len(b); n > 0 && b[n-1] == 10 {
		b = b[:n-1] // the server appends a newline to every frame
	}
	p := &packets.Packet{}
	return p, proto.Unmarshal(b, p)
}

func waitFor(c *websocket.Conn, ok func(*packets.Packet) bool) error {
	for {
		p, err := read(c)
		if err != nil {
			return err
		}
		if _, deny := p.Msg.(*packets.Packet_DenyResponse); deny {
			return fmt.Errorf("denied: %s", p.GetDenyResponse().Reason)
		}
		if ok(p) {
			return nil
		}
	}
}

func bot(i int, stop <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	c, _, err := websocket.DefaultDialer.Dial(*url, nil)
	if err != nil {
		fmt.Println("dial:", err)
		return
	}
	defer c.Close()
	isOk := func(p *packets.Packet) bool { _, ok := p.Msg.(*packets.Packet_OkResponse); return ok }
	var myId uint64
	if err := waitFor(c, func(p *packets.Packet) bool {
		m, ok := p.Msg.(*packets.Packet_Id)
		if ok {
			myId = m.Id.Id
		}
		return ok
	}); err != nil {
		fmt.Println("id:", err)
		return
	}
	name := fmt.Sprintf("%s%d", *prefix, i)
	if !*burst {
		authMu.Lock()
	}
	send(c, &packets.Packet{Msg: &packets.Packet_RegisterRequest{RegisterRequest: &packets.RegisterRequestMessage{Username: name, Password: "loadtest-pw", Color: brightColor()}}})
	if err := waitFor(c, isOk); err != nil && !strings.Contains(err.Error(), "exists") && !strings.Contains(err.Error(), "taken") {
		fmt.Println("register:", err)
	}
	send(c, &packets.Packet{Msg: &packets.Packet_LoginRequest{LoginRequest: &packets.LoginRequestMessage{Username: name, Password: "loadtest-pw"}}})
	err = waitFor(c, isOk)
	if !*burst {
		authMu.Unlock()
	}
	if err != nil {
		fmt.Println("login:", err)
		return
	}
	inGame.Add(1)

	go func() {
		var myX, myY float64
		lastNear := map[uint64]time.Time{}
		for {
			p, err := read(c)
			if err != nil {
				return
			}
			received.Add(1)
			if m, ok := p.Msg.(*packets.Packet_Player); ok {
				if m.Player.Id == myId {
					myX, myY = m.Player.X, m.Player.Y
				} else {
					dx, dy := m.Player.X-myX, m.Player.Y-myY
					now := time.Now()
					if dx*dx+dy*dy < nearDistance*nearDistance {
						if t, seen := lastNear[m.Player.Id]; seen && now.Sub(t) < 2*time.Second {
							mu.Lock()
							nearGaps = append(nearGaps, float64(now.Sub(t).Microseconds())/1000)
							mu.Unlock()
						}
						lastNear[m.Player.Id] = now
					} else {
						delete(lastNear, m.Player.Id)
					}
				}
			}
			if m, ok := p.Msg.(*packets.Packet_Chat); ok && strings.HasPrefix(m.Chat.Msg, "t=") {
				if ns, err := strconv.ParseInt(m.Chat.Msg[2:], 10, 64); err == nil {
					mu.Lock()
					lat = append(lat, float64(time.Now().UnixNano()-ns)/1e6)
					mu.Unlock()
				}
			}
		}
	}()

	steer := time.NewTicker(250 * time.Millisecond)
	chat := time.NewTicker(time.Second)
	defer steer.Stop()
	defer chat.Stop()
	dir := rand.Float64() * 6.283
	for {
		select {
		case <-stop:
			send(c, &packets.Packet{Msg: &packets.Packet_Disconnect{Disconnect: &packets.DisconnectMessage{Reason: "done"}}})
			return
		case <-steer.C:
			dir += rand.Float64() - 0.5
			send(c, &packets.Packet{Msg: &packets.Packet_PlayerDirection{PlayerDirection: &packets.PlayerDirectionMessage{Direction: dir}}})
		case <-chat.C:
			if i%5 == 0 { // a fifth of the bots chat, so chat doesn't dominate traffic
				send(c, &packets.Packet{Msg: &packets.Packet_Chat{Chat: &packets.ChatMessage{Msg: "t=" + strconv.FormatInt(time.Now().UnixNano(), 10)}}})
			}
		}
	}
}

func main() {
	flag.Parse()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < *n; i++ {
		wg.Add(1)
		go bot(i, stop, &wg)
		if !*burst {
			time.Sleep(20 * time.Millisecond)
		}
	}
	time.Sleep(3 * time.Second) // let initial spore batches settle
	received.Store(0)
	bytesIn.Store(0)
	mu.Lock()
	lat = nil
	nearGaps = nil
	mu.Unlock()
	time.Sleep(*dur)
	secs := dur.Seconds()
	close(stop)
	wg.Wait()
	sort.Float64s(lat)
	sort.Float64s(nearGaps)
	pctOf := func(xs []float64, p float64) float64 {
		if len(xs) == 0 {
			return 0
		}
		return xs[int(p*float64(len(xs)-1))]
	}
	pct := func(p float64) float64 { return pctOf(lat, p) }
	fmt.Printf("bots_in_game=%d msgs_per_sec_total=%.0f msgs_per_sec_per_bot=%.0f kB_per_sec_total=%.0f chat_samples=%d latency_ms p50=%.1f p95=%.1f p99=%.1f\n",
		inGame.Load(), float64(received.Load())/secs, float64(received.Load())/secs/float64(max(inGame.Load(), 1)), float64(bytesIn.Load())/secs/1024, len(lat), pct(.5), pct(.95), pct(.99))
	fmt.Printf("near_update_gap_ms samples=%d p50=%.0f p95=%.0f p99=%.0f\n", len(nearGaps), pctOf(nearGaps, .5), pctOf(nearGaps, .95), pctOf(nearGaps, .99))
}

// brightColor returns a random fully-bright, saturated colour packed as 0xRRGGBBAA,
// which the server accepts (it rejects dark or grey blobs).
func brightColor() int32 {
	channels := []uint32{255, uint32(rand.Intn(256)), 0}
	rand.Shuffle(len(channels), func(i, j int) { channels[i], channels[j] = channels[j], channels[i] })
	return int32(channels[0]<<24 | channels[1]<<16 | channels[2]<<8 | 0xff)
}
