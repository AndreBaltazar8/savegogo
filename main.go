package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"io/ioutil"
	"log"
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type feeder struct {
	numFeeds int
	nextFeed time.Time
}

type game struct {
	birthTime    time.Time
	deathTime    time.Time
	hungerAmount int
	hungerTicker *time.Ticker
	feeders      map[string]*feeder
	players      int
	totalFeeds   int
}

type message struct {
	Type        string `json:"type"`
	PlayerCount int    `json:"player_count"`
	FeederCount int    `json:"feeder_count"`
	BirthTime   int64  `json:"birth_time"`
	DeathTime   int64  `json:"death_time"`
	Hunger      int    `json:"hunger"`
	NextFeed    int64  `json:"next_feed"`
	TotalFeeds  int    `json:"total_feeds"`
}

type player struct {
	id     string
	conn   *websocket.Conn
	feeder *feeder
}

func (p *player) updateGameState() {
	var playerNextFeed int64
	f := p.feeder
	if f != nil {
		playerNextFeed = int64(f.nextFeed.Sub(time.Now()).Seconds())
	}

	err := p.conn.WriteJSON(message{
		Type:        "update_game_state",
		PlayerCount: gameState.players,
		FeederCount: len(gameState.feeders),
		BirthTime:   gameState.birthTime.Unix(),
		DeathTime:   gameState.deathTime.Unix(),
		Hunger:      gameState.hungerAmount,
		NextFeed:    playerNextFeed,
		TotalFeeds:  gameState.totalFeeds,
	})
	if err != nil {
		log.Println("failed to send game state update...", err)
	}
}

func announceGameState() {
	scheduleSaveState()
	gameMutex.RLock()
	for _, p := range players {
		p.updateGameState()
	}
	gameMutex.RUnlock()
}

var saveMutex = &sync.Mutex{}
var lastSave time.Time = time.Now()

func scheduleSaveState() {
	saveMutex.Lock()
	defer saveMutex.Unlock()

	if time.Now().Sub(lastSave) > time.Second {
		lastSave = time.Now()
		saveState()
	}
}

type fullGameState struct {
	BirthTime    int64                  `json:"birth_time"`
	DeathTime    int64                  `json:"death_time"`
	HungerAmount int                    `json:"hunger"`
	TotalFeeds   int                    `json:"total_feeds"`
	Feeders      map[string]feederState `json:"feeders"`
}

type feederState struct {
	NumFeeds int   `json:"c"`
	NextFeed int64 `json:"n"`
}

func saveState() {
	gameMutex.RLock()
	defer gameMutex.RUnlock()

	state := fullGameState{
		BirthTime:    gameState.birthTime.Unix(),
		DeathTime:    gameState.deathTime.Unix(),
		HungerAmount: gameState.hungerAmount,
		TotalFeeds:   gameState.totalFeeds,
		Feeders:      make(map[string]feederState),
	}
	for id, f := range gameState.feeders {
		state.Feeders[id] = feederState{
			NumFeeds: f.numFeeds,
			NextFeed: f.nextFeed.Unix(),
		}
	}

	b, err := json.Marshal(state)
	if err != nil {
		log.Println("failed to marshal state:", err)
		return
	}

	err = ioutil.WriteFile("data/state.json", b, 0644)
	if err != nil {
		log.Println("failed to write state:", err)
		return
	}
}

func restoreState() {
	b, err := ioutil.ReadFile("data/state.json")
	if err != nil {
		log.Println("failed to read state:", err)
		return
	}

	var state fullGameState
	err = json.Unmarshal(b, &state)
	if err != nil {
		log.Println("failed to unmarshal state:", err)
		return
	}

	gameMutex.Lock()
	defer gameMutex.Unlock()

	gameState.birthTime = time.Unix(state.BirthTime, 0)
	gameState.deathTime = time.Unix(state.DeathTime, 0)
	gameState.hungerAmount = state.HungerAmount
	gameState.totalFeeds = state.TotalFeeds
	gameState.feeders = make(map[string]*feeder)
	for id, f := range state.Feeders {
		gameState.feeders[id] = &feeder{
			numFeeds: f.NumFeeds,
			nextFeed: time.Unix(f.NextFeed, 0),
		}
	}
}

var players = make(map[string]*player)
var gameMutex = &sync.RWMutex{}

const initialTimeAliveDays = 1
const hungerTick = 6 * time.Minute

var birth = time.Now()
var gameState = game{
	birthTime:    birth,
	deathTime:    birth.Add(time.Hour * initialTimeAliveDays * 24),
	hungerAmount: 10,
	hungerTicker: time.NewTicker(hungerTick),
	feeders:      make(map[string]*feeder),
	totalFeeds:   0,
}

func processDisconnect(player *player) {
	gameMutex.Lock()
	delete(players, player.id)
	gameState.players--
	gameMutex.Unlock()
	announceGameState()
}

func createPlayer(conn *websocket.Conn) *player {
	buf := make([]byte, 10)
	n, err := rand.Read(buf)
	for n != 10 || err != nil {
		n, err = rand.Read(buf)
	}

	player := &player{
		id:   base64.StdEncoding.EncodeToString(buf),
		conn: conn,
	}
	return player
}

func initPlayer(conn *websocket.Conn, sourceIP string) {
	var player = createPlayer(conn)
	player.feeder = gameState.feeders[sourceIP]
	announcePlayer(player)

	for {
		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			processDisconnect(player)
			return
		}

		if msgType == websocket.TextMessage {
			var inboundMsg *message
			err := json.Unmarshal(msg, &inboundMsg)
			if err != nil {
				processDisconnect(player)
				return
			}

			if inboundMsg.Type == "feed" {
				gameMutex.Lock()
				// check if game ended
				if gameState.deathTime.Before(time.Now()) {
					gameMutex.Unlock()
					return
				}

				f := player.feeder
				if f == nil {
					f = gameState.feeders[sourceIP]
				}
				if f != nil {
					if f.nextFeed.After(time.Now()) {
						// feed too soon
						gameMutex.Unlock()
						continue
					}

					f.numFeeds++
					multiplier := math.Pow(2, float64(f.numFeeds-1))
					f.nextFeed = time.Now().Add(time.Hour * time.Duration(multiplier))
				} else {
					f = &feeder{
						numFeeds: 1,
						nextFeed: time.Now().Add(time.Hour),
					}
					gameState.feeders[sourceIP] = f
					player.feeder = f
				}

				// 10 = 60m
				// 9 = 54m
				// 8 = 48m
				// 7 = 42m
				// 6 = 36m
				// 5 = 30m
				// 4 = 24m
				// 3 = 18m
				// 2 = 12m
				// 1 = 6m
				// 0 = 5s
				feedSeconds := 5 // if no hunger then feed for 5 seconds
				if gameState.hungerAmount > 0 {
					feedSeconds = gameState.hungerAmount * 6 * 60 // if hunger is over 0, feed for each hunger point times 6 minutes
				}
				gameState.deathTime = gameState.deathTime.Add(time.Second * time.Duration(feedSeconds))
				gameState.totalFeeds++
				gameState.hungerAmount--
				gameState.hungerTicker.Reset(hungerTick)
				if gameState.hungerAmount < 0 {
					gameState.hungerAmount = 0
				}
				gameMutex.Unlock()
				announceGameState()
			}
		}
	}
}

func announcePlayer(p *player) {
	gameMutex.Lock()
	players[p.id] = p
	gameState.players++
	gameMutex.Unlock()
	announceGameState()
}

func main() {
	restoreState()

	go func() {
	gameLoop:
		for {
			select {
			case <-gameState.hungerTicker.C:
				if gameState.deathTime.Before(time.Now()) {
					break gameLoop
				}

				gameMutex.Lock()
				if gameState.hungerAmount < 10 {
					gameState.hungerAmount++
					gameMutex.Unlock()
					announceGameState()
				} else {
					gameState.hungerTicker.Stop()
					gameMutex.Unlock()
				}
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/game", func(w http.ResponseWriter, r *http.Request) {
		conn, _ := upgrader.Upgrade(w, r, nil) // error ignored for sake of simplicity
		var realIp string
		realIpValues := r.Header["X-Forwarded-For"]
		if realIpValues != nil {
			realIp = realIpValues[len(realIpValues)-1]
		} else {
			switch addr := conn.RemoteAddr().(type) {
			case *net.TCPAddr:
				realIp = addr.IP.String()
			}
			if realIp == "" {
				return
			}
		}
		go initPlayer(conn, realIp)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t, _ := template.ParseFiles("index.html")
		content, err := ioutil.ReadFile("static/dog.svg")
		if err != nil {
			log.Fatal(err)
		}

		err = t.Execute(w, template.HTML(content))
		if err != nil {
			http.Error(w, "Failed", 503)
			return
		}
	})

	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))

	srv := &http.Server{
		Addr:         ":4446",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
		Handler:      mux,
	}

	log.Println("Listening...")
	log.Fatal(srv.ListenAndServe())
}
