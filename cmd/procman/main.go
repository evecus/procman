package main

import (
	"log"
	"os"

	"github.com/procman/internal/api"
	"github.com/procman/internal/manager"
)

func main() {
	dataDir := os.Getenv("PROCMAN_DATA")
	if dataDir == "" {
		dataDir = "/data/services"
	}
	addr := os.Getenv("PROCMAN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	mgr, err := manager.New(dataDir)
	if err != nil {
		log.Fatalf("init manager: %v", err)
	}

	if err := api.StartServer(addr, mgr); err != nil {
		log.Fatalf("server: %v", err)
	}
}