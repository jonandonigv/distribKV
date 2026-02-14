package kvserver

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/jonandonigv/distribKV/pkg/common"
	pb "github.com/jonandonigv/distribKV/proto/kv"
)

// MakeClerk creates a new Clerk instance connected to all servers
func MakeClerk(servers []string) *Clerk {
	ck := &Clerk{
		servers:   servers,
		leaderId:  -1,
		clientId:  time.Now().UnixNano(),
		seqNum:    0,
		clients:   make(map[int]*common.Client),
		kvClients: make(map[int]pb.KVClient),
	}

	// Connect to all servers
	for _, addr := range ck.servers {
		serverId := deriveIdFromAddress(addr)

		client := common.NewClient(addr)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := client.Connect(ctx)
		cancel()

		if err != nil {
			log.Printf("Clerk: failed to connect to %s: %v", addr, err)
			continue
		}

		ck.clients[serverId] = client
		ck.kvClients[serverId] = pb.NewKVClient(client.Conn())
	}

	if len(ck.clients) == 0 {
		panic("Clerk: failed to connect to any server")
	}

	return ck
}

// deriveIdFromAddress extracts server ID from address (port %% 10000)
func deriveIdFromAddress(address string) int {
	parts := strings.Split(address, ":")
	if len(parts) != 2 {
		return 0
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	return port % 10000
}
