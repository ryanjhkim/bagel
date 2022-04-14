package bagel

import (
	"log"
	"math/rand"
	"net"
	"net/rpc"
	fchecker "project/fcheck"
	"project/util"
	"strings"
	"sync"
	"time"
)

type CoordConfig struct {
	ClientAPIListenAddr     string // client will know this and use it to contact coord
	WorkerAPIListenAddr     string // new joining workers will message this addr
	LostMsgsThresh          uint8  // fcheck
	StepsBetweenCheckpoints uint64
}

type SuperStepDone struct {
	messagesSent        uint64
	allVerticesInactive bool
}

type Coord struct {
	// Coord state may go here
	clientAPIListenAddr   string
	workerAPIListenAddr   string
	lostMsgsThresh        uint8
	workers               map[uint32]*rpc.Client // worker id --> worker connection
	queryWorkers          map[uint32]*rpc.Client // workers in use for current query - will be updated at start of query
	workersMutex          sync.Mutex
	lastCheckpointNumber  uint64
	lastWorkerCheckpoints map[uint32]uint64
	workerCounter         int
	workerCounterMutex    sync.Mutex
	checkpointFrequency   int
	superStepNumber       uint64
	workerDone            chan *rpc.Call
	allWorkersReady       chan bool
}

func NewCoord() *Coord {
	return &Coord{
		clientAPIListenAddr:   "",
		workerAPIListenAddr:   "",
		lostMsgsThresh:        0,
		lastWorkerCheckpoints: make(map[uint32]uint64),
		workers:               make(map[uint32]*rpc.Client),
		checkpointFrequency:   1,
	}
}

// this is the start of the query where coord notifies workers to initialize
// state for SuperStep 0
func (c *Coord) StartQuery(q Query, reply *QueryResult) error {
	log.Printf("StartQuery: received query: %v\n", q)

	if len(c.workers) == 0 {
		log.Printf("StartQuery: No workers available - will block until workers join\n")
	}

	for len(c.workers) == 0 {
		// block while no workers available
	}

	// go doesn't have a deep copy method :(
	c.queryWorkers = make(map[uint32]*rpc.Client)
	for k, v := range c.workers {
		c.queryWorkers[k] = v
	}

	// create new map of checkpoints for a new query which may have different number of workers
	c.lastWorkerCheckpoints = make(map[uint32]uint64)

	// call workers query handler
	startSuperStep := StartSuperStep{NumWorkers: uint8(len(c.queryWorkers))}
	numWorkers := len(c.queryWorkers)
	c.workerDone = make(chan *rpc.Call, numWorkers)
	c.allWorkersReady = make(chan bool, 1)

	log.Printf("StartQuery: computing query %v with %d workers ready!\n", q, numWorkers)

	go c.checkWorkersReady(numWorkers)
	for _, wClient := range c.queryWorkers {
		var result interface{}
		wClient.Go(
			"Worker.StartQuery", startSuperStep, &result,
			c.workerDone,
		)
	}

	select {
	case <-c.allWorkersReady:
		log.Printf("StartQuery: received all %d workers ready!\n", numWorkers)
	}

	// TODO: invoke another function to handle the rest of the request
	result, err := c.Compute()
	if err != nil {
		log.Printf("StartQuery: Compute returned err: %v", err)
	}

	reply.Query = q
	reply.Result = result

	c.queryWorkers = nil

	// return nil for no errors
	return nil
}

// check if all workers are notified by coord
func (c *Coord) checkWorkersReady(
	numWorkers int) {

	for {
		select {
		case call := <-c.workerDone:
			log.Printf("checkWorkersReady: received reply: %v\n", call.Reply)

			if call.Error != nil {
				log.Printf("checkWorkersReady: received error: %v\n", call.Error)
			} else {
				c.workerCounterMutex.Lock()
				c.workerCounter++
				c.workerCounterMutex.Unlock()
				log.Printf("checkWorkersReady: %d workers ready!\n", numWorkers)
			}

			if c.workerCounter == numWorkers {
				log.Printf("checkWorkersReady: sending all %d workers ready!\n", numWorkers)
				c.allWorkersReady <- true
				c.workerCounterMutex.Lock()
				c.workerCounter = 0
				c.workerCounterMutex.Unlock()
				return
			}
		}
	}
}

// TODO: test this!
func (c *Coord) UpdateCheckpoint(
	msg CheckpointMsg, reply *CheckpointMsg,
) error {
	log.Printf("UpdateCheckpoint: msg: %v\n", msg)
	// save the last SuperStep # checkpointed by this worker
	c.lastWorkerCheckpoints[msg.WorkerId] = msg.SuperStepNumber

	// update global SuperStep # if needed
	allWorkersUpdated := true
	for _, v := range c.lastWorkerCheckpoints {
		if v != msg.SuperStepNumber {
			allWorkersUpdated = false
			break
		}
	}
	log.Printf("UpdateCheckpoint: updated coord checkpoints map: %v\n", c.lastWorkerCheckpoints)

	if allWorkersUpdated {
		c.lastCheckpointNumber = msg.SuperStepNumber
	}

	*reply = msg
	return nil
}

func (c *Coord) Compute() (int, error) {
	// keep sending messages to workers, until everything has completed
	// need to make it concurrent; so put in separate channel

	numWorkers := len(c.queryWorkers)

	// TODO check if all workers are finished, currently returns placeholder result after 5 supersteps
	for i := 0; i < 5; i++ {
		// for {

		shouldCheckPoint := c.superStepNumber%uint64(c.checkpointFrequency) == 0

		// call workers query handler
		progressSuperStep := ProgressSuperStep{
			SuperStepNum: c.superStepNumber,
			IsCheckpoint: shouldCheckPoint,
		}

		//c.workerDone = make(chan *rpc.Call, numWorkers)
		//c.allWorkersReady = make(chan bool, 1)
		go c.checkWorkersReady(numWorkers)

		log.Printf("Compute: progressing super step # %d, should checkpoint %v \n",
			c.superStepNumber, shouldCheckPoint)

		for _, wClient := range c.queryWorkers {
			var result ProgressSuperStep
			wClient.Go(
				"Worker.ComputeVertices", progressSuperStep, &result,
				c.workerDone,
			)
		}

		// TODO: for testing workers joining during query, remove
		time.Sleep(3 * time.Second)

		select {
		case <-c.allWorkersReady:
			log.Printf("Compute: received all %d workers - Superstep %d is complete!\n", c.superStepNumber, numWorkers)
		}

		c.superStepNumber += 1

	}
	log.Printf("Compute: Query complete, result found\n")
	return -1, nil
}

func (c *Coord) restartCheckpoint() {
	checkpointNumber := c.lastCheckpointNumber
	log.Printf("RestartCheckpoint: restarting from checkpoint %v\n", checkpointNumber)

	restartSuperStep := RestartSuperStep{SuperStepNumber: checkpointNumber}

	numWorkers := len(c.queryWorkers)
	//c.workerDone = make(chan *rpc.Call, numWorkers)
	//allWorkersReady := make(chan bool, 1)

	// TODO: we need a wrapper to retry sending if the failed worker is not restarted yet
	go c.checkWorkersReady(numWorkers)
	for _, wClient := range c.queryWorkers {
		var result interface{}
		wClient.Go(
			"Worker.RevertToLastCheckpoint", restartSuperStep, &result,
			c.workerDone,
		)
	}
}

func (c *Coord) JoinWorker(w WorkerNode, reply *WorkerNode) error {
	log.Printf("JoinWorker: Adding worker %d\n", w.WorkerId)

	// TODO: needs to block while there is an ongoing query

	client, err := util.DialRPC(w.WorkerListenAddr)
	if err != nil {
		log.Printf(
			"JoinWorker: coord could not dial worker addr %v, err: %v\n",
			w.WorkerListenAddr, err,
		)
		return err
	}

	go c.monitor(w)

	if _, ok := c.queryWorkers[w.WorkerId]; ok {
		// joining worker is restarted process of failed worker used in current query
		log.Printf(
			"JoinWorker: Worker %d rejoined after failure\n",
			w.WorkerId)
		c.queryWorkers[w.WorkerId] = client

		checkpointNumber := c.lastCheckpointNumber
		log.Printf("JoinWorker: restarting failed worker from checkpoint: %v\n", checkpointNumber)

		restartSuperStep := RestartSuperStep{SuperStepNumber: checkpointNumber}
		var result interface{}
		client.Go(
			"Worker.RevertToLastCheckpoint", restartSuperStep, &result, c.workerDone)
		log.Printf("JoinWorker: called RPC to revert to last checkpoint %v for readded worker\n", checkpointNumber)

	} else {
		c.workers[w.WorkerId] = client
		log.Printf(
			"JoinWorker: New Worker %d successfully added. %d Workers joined\n",
			w.WorkerId, len(c.workers))
	}

	// return nil for no errors
	return nil
}

func listenWorkers(workerAPIListenAddr string) {

	wlisten, err := net.Listen("tcp", workerAPIListenAddr)
	if err != nil {
		log.Printf("listenWorkers: Error listening: %v\n", err)
	}
	log.Printf(
		"listenWorkers: Listening for workers at %v\n",
		workerAPIListenAddr,
	)

	for {
		conn, err := wlisten.Accept()
		if err != nil {
			log.Printf(
				"listenWorkers: Error accepting worker: %v\n", err,
			)
		}
		log.Printf("listenWorkers: accepted connection to worker\n")
		go rpc.ServeConn(conn) // blocks while serving connection until client hangs up
	}
}

func (c *Coord) monitor(w WorkerNode) {

	// get random port for heartbeats
	//hBeatLocalAddr, _ := net.ResolveUDPAddr("udp", strings.Split(c.WorkerAPIListenAddr, ":")[0]+":0")
	log.Printf(
		"monitor: Starting fchecker for Worker %d at %v\n", w.WorkerId,
		w.WorkerAddr,
	)

	epochNonce := rand.Uint64()

	notifyCh, _, err := fchecker.Start(
		fchecker.StartStruct{
			strings.Split(c.workerAPIListenAddr, ":")[0] + ":0",
			epochNonce,
			strings.Split(c.workerAPIListenAddr, ":")[0] + ":0",
			w.WorkerFCheckAddr,
			c.lostMsgsThresh, w.WorkerId,
		},
	)
	if err != nil || notifyCh == nil {
		log.Printf("monitor: fchecker failed to connect. notifyCh nil and/or received err: %v\n", err)
	}

	log.Printf("monitor: Fcheck for Worker %d running\n", w.WorkerId)
	for {
		select {
		case notify := <-notifyCh:
			log.Printf("monitor: Worker %v failed: %s\n", w.WorkerId, notify)
			c.restartCheckpoint() // TODO: add logic to join handling to accept a new process for the worker that failed
		}
	}
}

func listenClients(clientAPIListenAddr string) {

	wlisten, err := net.Listen("tcp", clientAPIListenAddr)
	if err != nil {
		log.Printf("listenClients: Error listening: %v\n", err)
	}
	log.Printf(
		"listenClients: Listening for clients at %v\n",
		clientAPIListenAddr,
	)

	for {
		conn, err := wlisten.Accept()
		if err != nil {
			log.Printf(
				"listenClients: Error accepting client: %v\n", err,
			)
		}
		log.Printf("listenClients: Accepted connection to client\n")
		go rpc.ServeConn(conn) // blocks while serving connection until client hangs up
	}
}

// Only returns when network or other unrecoverable errors occur
func (c *Coord) Start(
	clientAPIListenAddr string, workerAPIListenAddr string,
	lostMsgsThresh uint8, checkpointSteps uint64,
) error {

	c.clientAPIListenAddr = clientAPIListenAddr
	c.workerAPIListenAddr = workerAPIListenAddr
	c.lostMsgsThresh = lostMsgsThresh

	err := rpc.Register(c)
	if err != nil {
		log.Printf("Start: Coord could not register RPCs\n")
		util.CheckErr(err, "Start: Coord could not register RPCs")
	}

	log.Printf("Start: Ready to accepting RPCs from workers and clients\n")

	wg := sync.WaitGroup{}
	wg.Add(2)
	go listenWorkers(workerAPIListenAddr)
	go listenClients(clientAPIListenAddr)
	wg.Wait()

	// will never return
	return nil
}
