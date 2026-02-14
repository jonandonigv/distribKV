package kvserver

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jonandonigv/distribKV/pkg/common"
	pb "github.com/jonandonigv/distribKV/proto/kv"
)

// MakeClerk creates a new Clerk instance connected to all servers
func MakeClerk(servers []string, verbose bool) *Clerk {
	ck := &Clerk{
		servers:   servers,
		serverIds: make([]int, 0, len(servers)),
		leaderId:  -1,
		clientId:  time.Now().UnixNano(),
		seqNum:    0,
		clients:   make(map[int]*common.Client),
		kvClients: make(map[int]pb.KVClient),
		verbose:   verbose,
	}

	// Connect to all servers
	for _, addr := range ck.servers {
		serverId := deriveIdFromAddress(addr)
		ck.serverIds = append(ck.serverIds, serverId)

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

	// Sort server IDs for predictable order
	sort.Ints(ck.serverIds)

	if len(ck.clients) == 0 {
		panic("Clerk: failed to connect to any server")
	}

	return ck
}

// deriveIdFromAddress extracts server ID from address (port % 10000)
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

// Get retrieves the value for a key from the KV cluster.
// Retries until successful or after 1000 attempts (indicates total cluster failure).
// Panics if unable to complete.
func (ck *Clerk) Get(key string) string {
	// Get thread-safe sequence number
	ck.mu.Lock()
	ck.seqNum++
	seqNum := ck.seqNum
	ck.mu.Unlock()

	attempt := 0

	for {
		// Try cached leader first (optimization)
		if ck.leaderId >= 0 {
			if val, ok := ck.tryGet(ck.leaderId, key, seqNum); ok {
				return val
			}
		}

		// Try all servers in order by ID
		for _, serverId := range ck.serverIds {
			// Skip if we already tried the cached leader
			if serverId == ck.leaderId {
				continue
			}

			if val, ok := ck.tryGet(serverId, key, seqNum); ok {
				return val
			}
		}

		// All servers failed this round
		attempt++

		// Sanity check: panic after 1000 attempts
		if attempt >= 1000 {
			panic(fmt.Sprintf("Clerk: Get failed after 1000 attempts for key=%s", key))
		}

		// Calculate backoff and wait
		delay := calculateBackoff(attempt)

		if ck.verbose {
			log.Printf("Clerk: Get attempt %d failed for key=%s, retrying in %v",
				attempt, key, delay)
		}

		time.Sleep(delay)
	}
}

// tryGet attempts a single Get RPC to a specific server.
// Returns (value, true) on success, ("", false) on failure.
func (ck *Clerk) tryGet(serverId int, key string, seqNum int64) (string, bool) {
	client, ok := ck.kvClients[serverId]
	if !ok {
		return "", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.Get(ctx, &pb.GetRequest{
		Key:         key,
		ClientId:    ck.clientId,
		SequenceNum: seqNum,
	})

	if err != nil {
		// Network error or timeout
		if ck.verbose {
			log.Printf("Clerk: Get RPC error to server %d: %v", serverId, err)
		}
		return "", false
	}

	if resp.WrongLeader {
		// Update leader hint if provided
		ck.mu.Lock()
		if resp.LeaderId >= 0 {
			ck.leaderId = int(resp.LeaderId)
			if ck.verbose {
				log.Printf("Clerk: Learned leader is server %d", resp.LeaderId)
			}
		} else {
			ck.leaderId = -1 // Unknown
		}
		ck.mu.Unlock()
		return "", false
	}

	if resp.Success {
		// Cache this server as leader for future requests
		ck.mu.Lock()
		ck.leaderId = serverId
		ck.mu.Unlock()

		if ck.verbose {
			log.Printf("Clerk: Get succeeded on server %d", serverId)
		}

		return resp.Value, true
	}

	// Other error (shouldn't happen with current protocol)
	if ck.verbose {
		log.Printf("Clerk: Get failed on server %d: %s", serverId, resp.Error)
	}
	return "", false
}

// calculateBackoff computes exponential backoff delay.
// Attempt 1: 50ms, Attempt 2: 100ms, Attempt 3: 200ms, etc.
// Caps at 1 second.
func calculateBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return 50 * time.Millisecond
	}

	// Calculate: 50 * 2^(attempt-1) milliseconds
	delayMs := 50.0 * math.Pow(2, float64(attempt-1))

	// Cap at 1 second
	if delayMs > 1000 {
		delayMs = 1000
	}

	return time.Duration(delayMs) * time.Millisecond
}

// Put stores a key-value pair in the KV cluster.
// Retries until successful or after 1000 attempts.
// Panics if unable to complete.
func (ck *Clerk) Put(key, value string) {
	// Get thread-safe sequence number
	ck.mu.Lock()
	ck.seqNum++
	seqNum := ck.seqNum
	ck.mu.Unlock()

	attempt := 0

	for {
		// Try cached leader first (optimization)
		if ck.leaderId >= 0 {
			if ok := ck.tryPut(ck.leaderId, key, value, seqNum); ok {
				return
			}
		}

		// Try all servers in order by ID
		for _, serverId := range ck.serverIds {
			// Skip if we already tried the cached leader
			if serverId == ck.leaderId {
				continue
			}

			if ok := ck.tryPut(serverId, key, value, seqNum); ok {
				return
			}
		}

		// All servers failed this round
		attempt++

		// Sanity check: panic after 1000 attempts
		if attempt >= 1000 {
			panic(fmt.Sprintf("Clerk: Put failed after 1000 attempts for key=%s", key))
		}

		// Calculate backoff and wait
		delay := calculateBackoff(attempt)

		if ck.verbose {
			log.Printf("Clerk: Put attempt %d failed for key=%s, retrying in %v",
				attempt, key, delay)
		}

		time.Sleep(delay)
	}
}

// tryPut attempts a single Put RPC to a specific server.
// Returns true on success, false on failure.
func (ck *Clerk) tryPut(serverId int, key, value string, seqNum int64) bool {
	client, ok := ck.kvClients[serverId]
	if !ok {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.Put(ctx, &pb.PutRequest{
		Key:         key,
		Value:       value,
		ClientId:    ck.clientId,
		SequenceNum: seqNum,
	})

	if err != nil {
		// Network error or timeout
		if ck.verbose {
			log.Printf("Clerk: Put RPC error to server %d: %v", serverId, err)
		}
		return false
	}

	if resp.WrongLeader {
		// Update leader hint if provided
		ck.mu.Lock()
		if resp.LeaderId >= 0 {
			ck.leaderId = int(resp.LeaderId)
			if ck.verbose {
				log.Printf("Clerk: Learned leader is server %d", resp.LeaderId)
			}
		} else {
			ck.leaderId = -1 // Unknown
		}
		ck.mu.Unlock()
		return false
	}

	if resp.Success {
		// Cache this server as leader for future requests
		ck.mu.Lock()
		ck.leaderId = serverId
		ck.mu.Unlock()

		if ck.verbose {
			log.Printf("Clerk: Put succeeded on server %d", serverId)
		}
		return true
	}

	// Other error (shouldn't happen with current protocol)
	if ck.verbose {
		log.Printf("Clerk: Put failed on server %d: %s", serverId, resp.Error)
	}
	return false
}

// Append appends a value to a key in the KV cluster.
// Retries until successful or after 1000 attempts.
// Panics if unable to complete.
func (ck *Clerk) Append(key, value string) {
	// Get thread-safe sequence number
	ck.mu.Lock()
	ck.seqNum++
	seqNum := ck.seqNum
	ck.mu.Unlock()

	attempt := 0

	for {
		// Try cached leader first (optimization)
		if ck.leaderId >= 0 {
			if ok := ck.tryAppend(ck.leaderId, key, value, seqNum); ok {
				return
			}
		}

		// Try all servers in order by ID
		for _, serverId := range ck.serverIds {
			// Skip if we already tried the cached leader
			if serverId == ck.leaderId {
				continue
			}

			if ok := ck.tryAppend(serverId, key, value, seqNum); ok {
				return
			}
		}

		// All servers failed this round
		attempt++

		// Sanity check: panic after 1000 attempts
		if attempt >= 1000 {
			panic(fmt.Sprintf("Clerk: Append failed after 1000 attempts for key=%s", key))
		}

		// Calculate backoff and wait
		delay := calculateBackoff(attempt)

		if ck.verbose {
			log.Printf("Clerk: Append attempt %d failed for key=%s, retrying in %v",
				attempt, key, delay)
		}

		time.Sleep(delay)
	}
}

// tryAppend attempts a single Append RPC to a specific server.
// Returns true on success, false on failure.
func (ck *Clerk) tryAppend(serverId int, key, value string, seqNum int64) bool {
	client, ok := ck.kvClients[serverId]
	if !ok {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.Append(ctx, &pb.AppendRequest{
		Key:         key,
		Value:       value,
		ClientId:    ck.clientId,
		SequenceNum: seqNum,
	})

	if err != nil {
		// Network error or timeout
		if ck.verbose {
			log.Printf("Clerk: Append RPC error to server %d: %v", serverId, err)
		}
		return false
	}

	if resp.WrongLeader {
		// Update leader hint if provided
		ck.mu.Lock()
		if resp.LeaderId >= 0 {
			ck.leaderId = int(resp.LeaderId)
			if ck.verbose {
				log.Printf("Clerk: Learned leader is server %d", resp.LeaderId)
			}
		} else {
			ck.leaderId = -1 // Unknown
		}
		ck.mu.Unlock()
		return false
	}

	if resp.Success {
		// Cache this server as leader for future requests
		ck.mu.Lock()
		ck.leaderId = serverId
		ck.mu.Unlock()

		if ck.verbose {
			log.Printf("Clerk: Append succeeded on server %d", serverId)
		}
		return true
	}

	// Other error (shouldn't happen with current protocol)
	if ck.verbose {
		log.Printf("Clerk: Append failed on server %d: %s", serverId, resp.Error)
	}
	return false
}
