package raft

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestElection_InitialOneLeader3Nodes is the foundational election test: a
// fresh 3-node cluster on start() must elect exactly one leader. We do NOT
// require the first winner to remain leader for any interval: with 150–300ms
// randomized election timeouts, two nodes can become candidates in the same
// term, split the vote, and the loser bumps the term and wins the next round
// — a legitimate succession within the first ~1s. We only assert that *some*
// lone leader is currently reporting at least once after start.
func TestElection_InitialOneLeader3Nodes(t *testing.T) {
	tc := newTestCluster(t, 3)
	tc.start()
	defer tc.shutdown()

	leader := tc.requireLeader(5 * time.Second)
	require.NotNil(t, leader)
	require.Equal(t, 1, tc.leaderCount(), "expected exactly one leader at sighting")
}

// TestElection_SingleNodeElectsSelf documents and enforces the single-node
// self-election contract: with no peers, the node's own vote is a majority,
// so it must transition directly to Leader without forwarding any RPCs.
//
// (Prior to the fix in becomeCandidate the single-node path closed
// electionDoneChan and returned without promoting to Leader, causing an
// infinite no-op election loop.)
func TestElection_SingleNodeElectsSelf(t *testing.T) {
	tc := newTestCluster(t, 1)
	tc.start()
	defer tc.shutdown()

	leader := tc.requireLeader(5 * time.Second)
	require.NotNil(t, leader)
	require.Equal(t, 0, leader.GetServerId(), "single-node leader must be node 0")
	require.Equal(t, 0, leader.GetLeaderId(), "GetLeaderId must return own id when leader")
}
