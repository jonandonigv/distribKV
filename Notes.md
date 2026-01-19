# distrivKV notes

## Cap Theorem

Cap theorem states that in any distributed data store can provide at most two of the following three
guarantees.

- Consistency: every read receives the mopst recent write and error. Consistency means that all the
  same data at the same time, no matter which node they connect to. For this to happen, whenever data is
  written to one node, it must be instantly forwarded or replicated to all the other nodes in the system
  before the write is deemed 'successful'.
- Availability: Every request received by non-failing node in the system must result in a response,
  without the guarantee that it contains the most recent version of the data.
- Partition tolerance: The system continues to operate despite an arbitrary number of messages being
  dropped (or delayed) by the network.

When a network partition failure happens, it must decide whether to do one of the following:

- cancel the operation and thus decrease the availability but ensure consistency
- proceed with the operation and thus provide availability but risk inconsistency. This does not
  necessarily mean that system is highly available to its users.

Thus, if there is a network partition, one has to choose between consistency or availability.
During times of normal operations, a data store covers all three.

## Linearizability

Linearizability is a strong consistency model for concurrent and distributed systems. It provides
the ilusion that: Every operation on a shared object appears to occur atomically at a single point
in time and those points respect real-time ordering. In practicla terms, linearizability makes a
distributed key-value store behave as if:

- There is only one copy of the data
- Operations execute one at time
- The system has a global, instantaneous order of operations

## State machine replication

State machine replication (SMR) is a general method for implementing a fault-tolerant service by
replicating servers and coordinating client interactions with server replicas.

## Raft consensus

Raft is a consensus algorithm. Consensus is a fundamental problem in fault-tolerant distributed systems.
Consensus involves multiple servers agreeing on values. Once they reach a decision on a value, that decision
is final.
