package kvserver

// TODO: Implement key-value service on top of Raft
//
// Purpose: Expose Raft consensus as a simple key-value store
//
// Required Operations:
// - Get(key string) (value string, err error)
// - Put(key string, value string) error
// - Append(key string, value string) error
//
// Architecture:
//
// 1. KVServer struct:
//    - Embed or reference Raft instance
//    - State machine: map[string]string
//    - Duplication detection: map[int64]bool for client requests
//    - Pending operations: map waiting for commit notification
//
// 2. Command Structure:
//    type Op struct {
//        Type       string  // "Put", "Append", "Get"
//        Key        string
//        Value      string
//        ClientId   int64
//        SequenceId int64   // For deduplication
//    }
//
// 3. Request Handling:
//    - Leader: submit to Raft, wait for commit, apply to state machine
//    - Follower: reject or forward to leader (depending on design)
//    - Linearizability: each operation appears atomic
//
// 4. State Machine Application:
//    - Goroutine monitors Raft apply channel
//    - When entry committed: apply to kv map
//    - Notify waiting client requests
//    - Handle duplicate detection
//
// 5. Leader Discovery:
//    - Track current leader
//    - On wrong leader: redirect or return leader hint
//    - Client retry logic in clerk
//
// Concurrency:
// - Multiple clients can submit concurrently
// - Raft serializes operations via log
// - KVServer applies in order
//
// Error Handling:
// - Timeout: retry or return error
// - Wrong leader: redirect client
// - Duplicate: return cached result
