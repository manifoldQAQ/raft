package raft

//
// this is an outline of the API that raft must expose to
// the service (or tester). see comments below for
// each of these functions for more details.
//
// rf = Make(...)
//   create a new Raft server.
// rf.Start(command interface{}) (index, term, isleader)
//   start agreement on a new log entry
// rf.GetState() (term, isLeader)
//   ask a Raft for its current term, and whether it thinks it is leader
// ApplyMsg
//   each time a new entry is committed to the log, each Raft peer
//   should send an ApplyMsg to the service (or tester)
//   in the same server.
//

import (
	"bytes"
	"log"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

import "../labrpc"
import "../labgob"


//
// as each Raft peer becomes aware that successive log entries are
// committed, the peer should send an ApplyMsg to the service (or
// tester) on the same server, via the applyCh passed to Make(). set
// CommandValid to true to indicate that the ApplyMsg contains a newly
// committed log entry.
//
// in Lab 3 you'll want to send other kinds of messages (e.g.,
// snapshots) on the applyCh; at that point you can add fields to
// ApplyMsg, but set CommandValid to false for these other uses.
//
type ApplyMsg struct {
	CommandValid bool
	Command      interface{}
	CommandIndex int
}

const (
	LEADER = iota
	CANDIDATE
	FOLLOWER
)

const ElectionTimeout = 1000 * time.Millisecond
const HeartbeatTimeout = 100 * time.Millisecond


type LogEntry struct {
	Command interface{}
	Term int
}

//
// A Go object implementing a single Raft peer.
//
type Raft struct {
	mu        sync.Mutex          // Lock to protect shared access to this peer's state
	peers     []*labrpc.ClientEnd // RPC end points of all peers
	persister *Persister          // Object to hold this peer's persisted state
	me        int                 // this peer's index into peers[]
	dead      int32               // set by Kill()

	// election
	currentTerm int
	role int
	votedFor int
	leaderId int
	voteCnt int

	// log replication
	log []LogEntry
	commitIndex int
	lastApplied int
	nextIndex []int
	matchIndex []int

	// follower converts to candidate if heartbeat timeout expires
	lastHeartbeat time.Time
	// candidate initiates a new round if election timeout expires
	lastElection time.Time

	applyCond *sync.Cond
}

func (rf *Raft) becomeLeader() {
	DPrintf("[ELECTION][%v] Raft %v elected leader", rf.currentTerm, rf.me)
	rf.role = LEADER
	rf.leaderId = rf.me
	localTerm := rf.currentTerm
	for i := 0; i < len(rf.nextIndex); i++ {
		rf.nextIndex[i] = len(rf.log)
	}
	for i := 0; i < len(rf.matchIndex); i++ {
		rf.matchIndex[i] = 0
	}
	for i := range rf.peers {
		if i != rf.me {
			// broadcast initial heartbeat to declare election
			go rf.tryHeartbeat(i, localTerm)

			// initiate a log replication request, otherwise a newly
			// elected leader can't replicate logs if no client request
			// kicks in.
			localNextIndex := append([]int{}, rf.nextIndex...)
			localMatchIndex := append([]int{}, rf.matchIndex...)
			localCommitIndex := rf.commitIndex
			go rf.tryAppendEntries(i, localTerm, localNextIndex, localMatchIndex, localCommitIndex)
		}
	}
}

func (rf *Raft) becomeCandidate() {
	DPrintf("[ELECTION][%v->%v] Raft %v converts to candidate", rf.currentTerm, rf.currentTerm + 1, rf.me)
	rf.currentTerm++
	rf.role = CANDIDATE
	rf.votedFor = rf.me
	rf.leaderId = -1
	rf.voteCnt = 1
	rf.lastElection = time.Now()
	rf.persist()
	// broadcast vote requests
	for i := range rf.peers {
		if i != rf.me {
			go func(i int) {
				rf.mu.Lock()
				args := RequestVoteArgs{
					Term:         rf.currentTerm,
					CandidateId:  rf.me,
					LastLogIndex: len(rf.log) - 1,
					LastLogTerm:  rf.log[len(rf.log)-1].Term,
				}
				reply := RequestVoteReply{
					Term:        0,
					VoteGranted: false,
				}
				rf.mu.Unlock()
				if rf.sendRequestVote(i, &args, &reply) {
					rf.mu.Lock()
					if rf.currentTerm < reply.Term {
						rf.becomeFollower(reply.Term, -1)
					} else if rf.role == CANDIDATE &&
						rf.currentTerm == reply.Term &&
						reply.VoteGranted {
						rf.voteCnt++
						// convert to leader on quorum
						if rf.voteCnt > len(rf.peers) / 2 {
							rf.becomeLeader()
						}
					}
					rf.mu.Unlock()
				}
			}(i)
		}
	}
}

func (rf *Raft) becomeFollower(term int, leaderId int) {
	if leaderId == -1 {
		DPrintf("[ELECTION][%v] Raft %v converts to follower", term, rf.me)
	} else {
		DPrintf("[ELECTION][%v] Raft %v converts to follower under leader %v", term, rf.me, leaderId)
	}
	rf.currentTerm = term
	rf.role = FOLLOWER
	rf.votedFor = -1
	rf.leaderId = leaderId
	rf.lastHeartbeat = time.Now()
	rf.persist()
}

// return currentTerm and whether this server
// believes it is the leader.
func (rf *Raft) GetState() (int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.currentTerm, rf.role == LEADER
}

//
// save Raft's persistent state to stable storage,
// where it can later be retrieved after a crash and restart.
//
func (rf *Raft) persist() {
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	_ = e.Encode(rf.currentTerm)
	_ = e.Encode(rf.votedFor)
	_ = e.Encode(rf.log)
	data := w.Bytes()
	rf.persister.SaveRaftState(data)
}


//
// restore previously persisted state.
//
func (rf *Raft) readPersist(data []byte) {
	// bootstrap without any state
	if data == nil || len(data) < 1 {
		return
	}
	r := bytes.NewBuffer(data)
	d := labgob.NewDecoder(r)
	currentTerm := 0
	votedFor := 0
	log_ := make([]LogEntry, 0)
	if d.Decode(&currentTerm) != nil {
		panic("error decoding currentTerm")
	}
	if d.Decode(&votedFor) != nil {
		panic("error decoding votedFor")
	}
	if d.Decode(&log_) != nil {
		panic("error decoding log")
	}
	rf.currentTerm = currentTerm
	rf.votedFor = votedFor
	rf.log = log_
}


// The RPC mechanism here is not a good abstraction. Since the vanilla
// concept of RPC itself is synchronous, which does not quite fit into
// the context of Raft.
type AppendEntriesArgs struct {
	Term int
	LeaderId int
	PrevLogIndex int
	PrevLogTerm int
	Entries []LogEntry
	LeaderCommit int
}

type AppendEntriesReply struct {
	Term          int
	Success       bool
	NextIndexHint int
	TermHint      int
}

func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	// outdated RPC, simply ignore.
	if rf.currentTerm > args.Term {
		reply.Term = rf.currentTerm
		reply.Success = false
		return
	}

	// on a more recent RPC, convert to follower.
	if rf.currentTerm < args.Term {
		rf.becomeFollower(args.Term, args.LeaderId)
	}

	// a regular heartbeat (might be initial).
	if rf.role == LEADER {
		log.Printf("[%v] Conflicting leaders %v and %v", rf.currentTerm, rf.me, args.LeaderId)
		panic(nil)
	}
	if rf.role == CANDIDATE {
		rf.role = FOLLOWER
	}
	rf.leaderId = args.LeaderId
	rf.lastHeartbeat = time.Now()

	// mismatch at prevLogIndex
	if args.PrevLogIndex >= len(rf.log) {
		DPrintf("[REJECT] reject log replication, PrevLogIndex %v, LogLength %v", args.PrevLogIndex, len(rf.log))
		reply.Term = rf.currentTerm
		reply.Success = false
		reply.TermHint = rf.log[reply.NextIndexHint].Term
		reply.NextIndexHint = len(rf.log) - 1
		for reply.NextIndexHint > 0 &&
			rf.log[reply.NextIndexHint - 1].Term == reply.TermHint {
			reply.NextIndexHint--
		}
		return
	}
	if rf.log[args.PrevLogIndex].Term != args.PrevLogTerm {
		DPrintf("[REJECT] reject log replication, PrevLogIndex %v, LogLength %v, LeaderTerm %v, FollowerTerm %v",
			args.PrevLogIndex, len(rf.log), args.PrevLogTerm, rf.log[args.PrevLogIndex].Term)
		reply.Term = rf.currentTerm
		reply.Success = false
		reply.TermHint = rf.log[reply.NextIndexHint].Term
		reply.NextIndexHint = len(rf.log) - 1
		for reply.NextIndexHint > 0 &&
			rf.log[reply.NextIndexHint - 1].Term == reply.TermHint {
			reply.NextIndexHint--
		}
		return
	}

	// append logs
	for i, entry := range args.Entries {
		logIndex := args.PrevLogIndex + i + 1
		if logIndex == len(rf.log) {
			rf.log = append(rf.log, entry)
		} else if rf.log[logIndex].Term != entry.Term {
			rf.log = rf.log[:logIndex]
			rf.log = append(rf.log, entry)
		}
	}
	// TODO not necessary to persist every single time
	rf.persist()

	// update commit index
	if args.LeaderCommit > rf.commitIndex {
		if args.LeaderCommit > args.PrevLogIndex + len(args.Entries) {
			rf.commitIndex = args.PrevLogIndex + len(args.Entries)
		} else {
			rf.commitIndex = args.LeaderCommit
		}
	}

	if rf.commitIndex > rf.lastApplied {
		rf.applyCond.Signal()
	}

	reply.Term = rf.currentTerm
	reply.Success = true
}

func (rf *Raft) sendAppendEntries(server int, args *AppendEntriesArgs, reply *AppendEntriesReply) bool {
	ok := rf.peers[server].Call("Raft.AppendEntries", args, reply)
	return ok
}

func (rf *Raft) tryHeartbeat(i int, localTerm int) {
	rf.mu.Lock()
	args := AppendEntriesArgs{
		Term:         localTerm,
		LeaderId:     rf.me,
		PrevLogIndex: len(rf.log) - 1,
		PrevLogTerm:  rf.log[len(rf.log)-1].Term,
		Entries:      make([]LogEntry, 0),
		LeaderCommit: rf.commitIndex,
	}
	reply := AppendEntriesReply{
		Term:    0,
		Success: false,
	}
	rf.mu.Unlock()
	if rf.sendAppendEntries(i, &args, &reply) {
		rf.mu.Lock()
		defer rf.mu.Unlock()
		if rf.currentTerm < reply.Term {
			rf.becomeFollower(reply.Term, -1)
		}
	}
}

func (rf *Raft) tryAppendEntries(i int, localTerm int, localNextIndex []int, localMatchIndex []int, localCommitIndex int) {
	rf.mu.Lock()
	if len(rf.log) - 1 >= localNextIndex[i] {
		prevLogIndex := localNextIndex[i] - 1
		lastLogIndex := len(rf.log) - 1
		args := AppendEntriesArgs{
			Term:         localTerm,
			LeaderId:     rf.me,
			PrevLogIndex: prevLogIndex,
			PrevLogTerm:  rf.log[prevLogIndex].Term,
			Entries:      append([]LogEntry{}, rf.log[localNextIndex[i]:]...),
			LeaderCommit: localCommitIndex,
		}
		rf.mu.Unlock()

		for !rf.killed() {
			reply := AppendEntriesReply{
				Term:    0,
				Success: false,
			}
			if !rf.sendAppendEntries(i, &args, &reply) {
				continue
			}
			rf.mu.Lock()
			if reply.Term > rf.currentTerm {
				rf.becomeFollower(reply.Term, -1)
				rf.mu.Unlock()
				break
			} else if reply.Term != rf.currentTerm || rf.role != LEADER {
				rf.mu.Unlock()
				break
			} else if reply.Success {
				localNextIndex[i] = lastLogIndex + 1
				localMatchIndex[i] = lastLogIndex
				DPrintf("[REPLICATION][%v] Log [%v,%v) replicated to %v from leader %v",
					rf.currentTerm, args.PrevLogIndex + 1, args.PrevLogIndex + 1 + len(args.Entries), i, rf.me)
				rf.mu.Unlock()
				break
			} else {
				// back by N
				nextIndexHint := math.MaxInt32
				for i := len(rf.log) - 1; i >= 0; i-- {
					if rf.log[i].Term == reply.TermHint {
						nextIndexHint = i
					}
				}
				if nextIndexHint == math.MaxInt32 {
					nextIndexHint = reply.NextIndexHint
				}
				nextIndexHint = Max(nextIndexHint, 1)
				localNextIndex[i] = nextIndexHint
				prevLogIndex = localNextIndex[i] - 1
				lastLogIndex = len(rf.log) - 1
				args.PrevLogIndex = prevLogIndex
				args.PrevLogTerm = rf.log[prevLogIndex].Term
				args.Entries = append([]LogEntry{}, rf.log[localNextIndex[i]:]...)
				args.LeaderCommit = localCommitIndex
				rf.mu.Unlock()
				// continues to send AppendEntries RPC
			}
		}
	} else {
		rf.mu.Unlock()
	}
	// check if commitIndex can be advanced
	rf.mu.Lock()
	for i := 0; i < len(rf.peers); i++ {
		rf.nextIndex[i] = Max(rf.nextIndex[i], localNextIndex[i])
		rf.matchIndex[i] = Max(rf.matchIndex[i], localMatchIndex[i])
	}
	for N := len(rf.log) - 1; N >= rf.commitIndex + 1; N-- {
		cnt := 1
		for i, idx := range rf.matchIndex {
			if i != rf.me && idx >= N {
				cnt++
			}
		}
		if cnt > len(rf.peers) / 2 && rf.log[N].Term == localTerm {
			rf.commitIndex = N
			DPrintf("[COMMIT][%v] CommitIndex incremented to %v", rf.currentTerm, rf.commitIndex)
			break
		}
	}
	// notify the applyCh
	if rf.commitIndex > rf.lastApplied {
		rf.applyCond.Signal()
	}
	rf.mu.Unlock()
}

type RequestVoteArgs struct {
	Term int
	CandidateId int
	LastLogIndex int
	LastLogTerm int
}

type RequestVoteReply struct {
	Term int
	VoteGranted bool
}

func (rf *Raft) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	// outdated RPC, simply ignore.
	if rf.currentTerm > args.Term {
		reply.Term = rf.currentTerm
		reply.VoteGranted = false
		return
	}

	// on a more recent RPC, convert to follower.
	if rf.currentTerm < args.Term {
		rf.becomeFollower(args.Term, -1)
	}

	// grant vote
	if (rf.votedFor == -1 || rf.votedFor == args.CandidateId) &&
		(rf.log[len(rf.log)-1].Term < args.LastLogTerm ||
			(rf.log[len(rf.log)-1].Term == args.LastLogTerm && len(rf.log) - 1 <= args.LastLogIndex)) {
		rf.votedFor = args.CandidateId
		rf.lastHeartbeat = time.Now()
		rf.persist()
		reply.Term = rf.currentTerm
		reply.VoteGranted = true
		DPrintf("[ELECTION][%v] Raft %v granted vote to Raft %v", rf.currentTerm, rf.me, args.CandidateId)
		return
	}

	// rejects vote
	reply.Term = rf.currentTerm
	reply.VoteGranted = false
	return
}

//
// example code to send a RequestVote RPC to a server.
// server is the index of the target server in rf.peers[].
// expects RPC arguments in args.
// fills in *reply with RPC reply, so caller should
// pass &reply.
// the types of the args and reply passed to Call() must be
// the same as the types of the arguments declared in the
// handler function (including whether they are pointers).
//
// The labrpc package simulates a lossy network, in which servers
// may be unreachable, and in which requests and replies may be lost.
// Call() sends a request and waits for a reply. If a reply arrives
// within a timeout interval, Call() returns true; otherwise
// Call() returns false. Thus Call() may not return for a while.
// A false return can be caused by a dead server, a live server that
// can't be reached, a lost request, or a lost reply.
//
// Call() is guaranteed to return (perhaps after a delay) *except* if the
// handler function on the server side does not return.  Thus there
// is no need to implement your own timeouts around Call().
//
// look at the comments in ../labrpc/labrpc.go for more details.
//
// if you're having trouble getting RPC to work, check that you've
// capitalized all field names in structs passed over RPC, and
// that the caller passes the address of the reply struct with &, not
// the struct itself.
//
func (rf *Raft) sendRequestVote(server int, args *RequestVoteArgs, reply *RequestVoteReply) bool {
	ok := rf.peers[server].Call("Raft.RequestVote", args, reply)
	return ok
}


//
// the service using Raft (e.g. a k/v server) wants to start
// agreement on the next command to be appended to Raft's log. if this
// server isn't the leader, returns false. otherwise start the
// agreement and return immediately. there is no guarantee that this
// command will ever be committed to the Raft log, since the leader
// may fail or lose an election. even if the Raft instance has been killed,
// this function should return gracefully.
//
// the first return value is the index that the command will appear at
// if it's ever committed. the second return value is the current
// term. the third return value is true if this server believes it is
// the leader.
//
func (rf *Raft) Start(command interface{}) (int, int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	if rf.role != LEADER {
		return 0, 0, false
	}

	rf.log = append(rf.log, LogEntry{
		Command: command,
		Term:    rf.currentTerm,
	})
	rf.persist()

	localTerm := rf.currentTerm
	for i := range rf.peers {
		if i != rf.me {
			localNextIndex := append([]int{}, rf.nextIndex...)
			localMatchIndex := append([]int{}, rf.matchIndex...)
			localCommitIndex := rf.commitIndex
			go rf.tryAppendEntries(i, localTerm, localNextIndex, localMatchIndex, localCommitIndex)
		}
	}

	return len(rf.log) - 1, localTerm, true
}

//
// the tester doesn't halt goroutines created by Raft after each test,
// but it does call the Kill() method. your code can use killed() to
// check whether Kill() has been called. the use of atomic avoids the
// need for a lock.
//
// the issue is that long-running goroutines use memory and may chew
// up CPU time, perhaps causing later tests to fail and generating
// confusing debug output. any goroutine with a long-running loop
// should call killed() to check whether it should stop.
//
func (rf *Raft) Kill() {
	atomic.StoreInt32(&rf.dead, 1)
	// Your code here, if desired.
}

func (rf *Raft) killed() bool {
	z := atomic.LoadInt32(&rf.dead)
	return z == 1
}

//
// the service or tester wants to create a Raft server. the ports
// of all the Raft servers (including this one) are in peers[]. this
// server's port is peers[me]. all the servers' peers[] arrays
// have the same order. persister is a place for this server to
// save its persistent state, and also initially holds the most
// recent saved state, if any. applyCh is a channel on which the
// tester or service expects Raft to send ApplyMsg messages.
// Make() must return quickly, so it should start goroutines
// for any long-running work.
//
func Make(peers []*labrpc.ClientEnd, me int,
	persister *Persister, applyCh chan ApplyMsg) *Raft {
	rf := &Raft{}
	rf.peers = peers
	rf.persister = persister
	rf.me = me
	rf.applyCond = sync.NewCond(&rf.mu)
	rf.role = FOLLOWER
	rf.currentTerm = 0
	rf.leaderId = -1
	rf.voteCnt = 0
	rf.currentTerm = 0
	rf.votedFor = -1
	rf.log = make([]LogEntry, 0)
	rf.log = append(rf.log, LogEntry{
		Command: nil,
		Term:    -1,
	})
	rf.commitIndex = 0
	rf.lastApplied = 0
	rf.nextIndex = make([]int, len(rf.peers))
	for i := 0; i < len(rf.nextIndex); i++ {
		rf.nextIndex[i] = len(rf.log)
	}
	rf.matchIndex = make([]int, len(rf.peers))

	// Your initialization code here (2A, 2B, 2C).
	// initialize from state persisted before a crash
	rf.readPersist(persister.ReadRaftState())
	rf.lastHeartbeat = time.Now()

	// randomly kick in to see if an election is needed.
	go func() {
		for !rf.killed() {
			timeout := ElectionTimeout.Milliseconds() + 2 * rand.Int63() % ElectionTimeout.Milliseconds()
			time.Sleep(time.Duration(timeout) * time.Millisecond)
			rf.mu.Lock()
			if rf.role == FOLLOWER && time.Now().Sub(rf.lastHeartbeat) > ElectionTimeout {
				// convert to candidate & start a new election
				rf.becomeCandidate()
			} else if rf.role == CANDIDATE && time.Now().Sub(rf.lastElection) > ElectionTimeout {
				// split vote
				rf.becomeCandidate()
			}
			rf.mu.Unlock()
		}
	}()

	// check if heartbeat msg is necessary
	go func() {
		for !rf.killed() {
			time.Sleep(HeartbeatTimeout)
			rf.mu.Lock()
			if rf.role == LEADER {
				localTerm := rf.currentTerm
				for i := range rf.peers {
					if i != rf.me {
						go rf.tryHeartbeat(i, localTerm)
					}
				}
			}
			rf.mu.Unlock()
		}
	}()

	// loop that send committed entries to applyCh
	go func() {
		for !rf.killed() {
			rf.mu.Lock()
			rf.applyCond.Wait()
			entriesToApply := rf.log[rf.lastApplied + 1: rf.commitIndex + 1]
			lastApplied := rf.lastApplied
			commitIndex := rf.commitIndex
			rf.mu.Unlock()
			for i, entry := range entriesToApply {
				applyCh <- ApplyMsg{
					CommandValid: true,
					Command:      entry.Command,
					CommandIndex: lastApplied + i + 1,
				}
			}
			rf.mu.Lock()
			rf.lastApplied = commitIndex
			rf.mu.Unlock()
		}
	}()

	return rf
}
