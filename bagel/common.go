package bagel

import "net/rpc"

// constants are used as msgType for the messages
const (
	PAGE_RANK            = "PageRank"
	SHORTEST_PATH        = "ShortestPath"
	SHORTEST_PATH_SOURCE = "ShortestPathSource"
	SHORTEST_PATH_DEST   = "ShortestPathDestination"
)

type WorkerNode struct {
	WorkerConfigId   uint32
	WorkerLogicalId  uint32
	WorkerAddr       string
	WorkerFCheckAddr string
	WorkerListenAddr string
	IsReplica        bool
	//Client           *rpc.Client
}

type StartSuperStep struct {
	NumWorkers      uint8
	WorkerDirectory WorkerDirectory
	WorkerLogicalId uint32
	ReplicaAddr     string
	Query           Query
	IsReplica       bool
}

type StartSuperStepResult struct {
	WorkerLogicalId uint32
	Vertices        []uint64
}

type ProgressSuperStep struct {
	SuperStepNum uint64
	IsCheckpoint bool
	IsRestart    bool
}

type VertexMessages map[uint64][]Message

type WorkerVertices map[uint32][]uint64

type VertexMessagesRPC struct {
	vertexMessages []Message
}

type ProgressSuperStepResult struct {
	SuperStepNum uint64
	IsCheckpoint bool
	IsActive     bool
	CurrentValue interface{}
	// experimental
	Messages VertexMessages
}

type RestartSuperStep struct {
	SuperStepNumber uint64
	WorkerDirectory WorkerDirectory
	NumWorkers      uint8
	Query           Query
}

//type UpdateMainReplica struct {
//	LogicalId  uint32
//	Worker
//	//WorkerPair FailoverQueryWorker
//}

type CheckpointMsg struct {
	SuperStepNumber uint64
	WorkerId        uint32
}

type Query struct {
	ClientId  string
	QueryType string   // PageRank or ShortestPath
	Nodes     []uint64 // if PageRank, will have 1 vertex, if shortestpath, will have [start, end]
	Graph     string   // graph to use - will always be google for now
	TableName string
}

type QueryResult struct {
	Query  Query
	Result interface{} // client dynamically casts Result based on Query.QueryType:
	Error  string
	// float64 for pagerank, int for shortest path
}

type EndQuery struct {
}

// WorkerDirectory maps worker ids to address (string)
type WorkerDirectory map[uint32]string

// WorkerCallBook maps worker ids to rpc clients (connections)
type WorkerCallBook map[uint32]*rpc.Client

type PromotedWorker struct {
	LogicalId uint32
	Worker    WorkerNode
}

type WorkerPool map[uint32]WorkerNode
