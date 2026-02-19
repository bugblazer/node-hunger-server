package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"server/internal/server"
	"server/internal/server/clients"

	"github.com/joho/godotenv"
)

// If the server is running in a Docker container, the data directory is always mounted here:
const (
	dockerMountedDataDir = "/gameserver/data"
)

type config struct {
	DataPath string
	Port     int
}

var (
	defaultConfig = &config{Port: 8080}
	configPath    = flag.String("config", ".env", "Path to the config file")
)

func loadConfig() *config {
	cfg := defaultConfig
	cfg.DataPath = os.Getenv("DATA_PATH")

	port, err := strconv.Atoi(os.Getenv("PORT"))
	if err != nil {
		log.Printf("Error parsing PORT, using %d", cfg.Port)
		return cfg
	}
	cfg.Port = port
	return cfg
}

func coalescePaths(fallbacks ...string) string {
	for i, path := range fallbacks {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			message := fmt.Sprintf("File/folder not found at %s", path)
			if i < len(fallbacks)-1 {
				log.Printf("%s - going to try %s", message, fallbacks[i+1])
			} else {
				log.Printf("%s - no more fallbacks to try", message)
			}
		} else {
			log.Printf("File/folder found at %s", path)
			return path
		}
	}
	return ""
}

func main() {
	flag.Parse()
	err := godotenv.Load(*configPath)
	cfg := defaultConfig
	if err != nil {
		log.Printf("Error loading config file, defaulting to %+v", defaultConfig)
	} else {
		cfg = loadConfig()
	}

	// Try to load the Docker-mounted data directory. If that fails,
	// fall back to the current directory
	cfg.DataPath = coalescePaths(cfg.DataPath, dockerMountedDataDir, ".")
	hub := server.NewHub(cfg.DataPath)

	// Define handler for WebSocket connections
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		hub.Serve(clients.NewWebSocketClient, w, r)
	})

	// Start the server
	go hub.Run()
	addr := fmt.Sprintf(":%d", cfg.Port)

	log.Printf("Starting server on %s", addr)

	err = http.ListenAndServe(addr, nil)
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
