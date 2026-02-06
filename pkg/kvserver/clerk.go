package kvserver

// TODO: Implement client library (clerk) for KV service
//
// Purpose: Client-side library for interacting with KV cluster
//
// Key Features:
//
// 1. Clerk Struct:
//    type Clerk struct {
//        servers   []string    // List of all server addresses
//        leaderId  int         // Cached leader hint
//        clientId  int64       // Unique client ID
//        seqNum    int64       // Monotonically increasing sequence number
//    }
//
// 2. Connection Management:
//    - Maintain connections to all servers
//    - Cache leader information
//    - Reconnect on failure
//
// 3. Request Methods:
//    - Get(key string) string
//    - Put(key string, value string)
//    - Append(key string, value string)
//
// 4. Retry Logic:
//    - On timeout: retry with same sequence number
//    - On wrong leader: update leader hint, retry
//    - Exponential backoff for failures
//    - At-least-once semantics (duplicates handled by server)
//
// 5. Sequence Numbers:
//    - Each operation gets unique (clientId, seqNum) pair
//    - Server detects duplicates and returns cached result
//    - Enables exactly-once semantics from client perspective
//
// 6. Leader Discovery:
//    - Try cached leader first
//    - On wrong leader error: try other servers
//    - Update leader cache on success
//
// Example Usage:
//    ck := kvserver.MakeClerk([]string{"localhost:10001", "localhost:10002", "localhost:10003"})
//    ck.Put("foo", "bar")
//    value := ck.Get("foo") // "bar"
//    ck.Append("foo", "-baz") // "bar-baz"
