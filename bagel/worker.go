package bagel

import (
	"bytes"
	"database/sql"
	"encoding/gob"
	"errors"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"net"
	"net/rpc"
	"os"
	fchecker "project/fcheck"
	"project/util"
	"sync"
)

// Message represents an arbitrary message sent during calculation
// value has a dynamic type based on the messageType
type Message struct {
	superStepNum   uint64
	sourceVertexId uint64
	destVertexId   uint64
	value          interface{}
}

type WorkerConfig struct {
	WorkerId              uint32
	CoordAddr             string
	WorkerAddr            string
	WorkerListenAddr      string
	FCheckAckLocalAddress string
}

type Worker struct {
	// Worker state may go here
	config        WorkerConfig
	SuperStep     SuperStep
	NextSuperStep SuperStep
	Vertices      map[uint64]Vertex
}

type Checkpoint struct {
	SuperStepNumber uint64
	CheckpointState map[uint64]VertexCheckpoint
}

type SuperStep struct {
	Id           uint64
	QueryType    string
	Messages     map[uint64][]Message
	Outgoing     map[uint32]uint64
	IsCheckpoint bool
}

func NewWorker(config WorkerConfig) *Worker {
	return &Worker{
		config: config,
		SuperStep: SuperStep{
			Id:           0,
			Messages:     nil,
			Outgoing:     nil,
			IsCheckpoint: false,
		},
		Vertices: make(map[uint64]Vertex),
	}
}

func (w *Worker) startFCheckHBeat(workerId uint32, ackAddress string) string {
	fmt.Printf("Starting fcheck for worker %d\n", workerId)

	fcheckConfig := fchecker.StartStruct{
		AckLocalIPAckLocalPort: ackAddress,
	}

	_, addr, err := fchecker.Start(fcheckConfig)

	if err != nil {
		fchecker.Stop()
		util.CheckErr(err, fmt.Sprintf("fchecker for Worker %d failed", workerId))
	}

	return addr
}

func dbSetup() (*sql.DB, error) {
	//goland:noinspection SqlDialectInspection
	const createCheckpoints string = `
	  CREATE TABLE IF NOT EXISTS checkpoints (
	  superStepNumber INTEGER NOT NULL PRIMARY KEY,
	  checkpointState BLOB NOT NULL
	  );`
	db, err := sql.Open("sqlite3", "checkpoints.db")
	if err != nil {
		fmt.Printf("Failed to open database: %v\n", err)
		return nil, err
	}

	if _, err := db.Exec(createCheckpoints); err != nil {
		fmt.Printf("Failed execute command: %v\n", err)
		return nil, err
	}

	return db, nil
}

func (w *Worker) checkpoint() Checkpoint {
	checkPointState := make(map[uint64]VertexCheckpoint)

	for k, v := range w.Vertices {
		checkPointState[k] = VertexCheckpoint{
			CurrentValue: v.currentValue,
			Messages:     v.messages,
			IsActive:     v.isActive,
		}
	}

	return Checkpoint{
		SuperStepNumber: w.SuperStep.Id,
		CheckpointState: checkPointState,
	}
}

func (w *Worker) storeCheckpoint(checkpoint Checkpoint) (Checkpoint, error) {
	db, err := dbSetup()
	if err != nil {
		os.Exit(1)
	}
	defer db.Close()

	var buf bytes.Buffer
	if err = gob.NewEncoder(&buf).Encode(checkpoint.CheckpointState); err != nil {
		fmt.Printf("encode error: %v\n", err)
	}

	_, err = db.Exec("INSERT INTO checkpoints VALUES(?,?)", checkpoint.SuperStepNumber, buf.Bytes())
	if err != nil {
		fmt.Printf("error inserting into db: %v\n", err)
	}
	fmt.Printf("inserted ssn: %v, buf: %v\n", checkpoint.SuperStepNumber, buf.Bytes())

	// notify coord about the latest checkpoint saved
	coordClient, err := util.DialRPC(w.config.CoordAddr)
	util.CheckErr(err, fmt.Sprintf("worker %v could not dial coord addr %v\n", w.config.WorkerAddr,
		w.config.CoordAddr))

	checkpointMsg := CheckpointMsg{
		SuperStepNumber: checkpoint.SuperStepNumber,
		WorkerId:        w.config.WorkerId,
	}

	var reply CheckpointMsg
	fmt.Printf("calling coord update cp: %v\n", checkpointMsg)
	err = coordClient.Call("Coord.UpdateCheckpoint", checkpointMsg, &reply)
	util.CheckErr(err, fmt.Sprintf("worker %v could not call UpdateCheckpoint", w.config.WorkerAddr))

	fmt.Printf("called coord update cp: %v\n", reply)

	return checkpoint, nil
}

func (w *Worker) retrieveCheckpoint(superStepNumber uint64) (Checkpoint, error) {
	db, err := dbSetup()
	if err != nil {
		os.Exit(1)
	}
	defer db.Close()

	res := db.QueryRow("SELECT * FROM checkpoints WHERE superStepNumber=?", superStepNumber)
	checkpoint := Checkpoint{}
	var buf []byte
	if err := res.Scan(&checkpoint.SuperStepNumber, &buf); err == sql.ErrNoRows {
		fmt.Printf("scan error: %v\n", err)
		return Checkpoint{}, err
	}
	fmt.Printf("buf: %v\n", buf)
	var checkpointState map[uint64]VertexCheckpoint
	err = gob.NewDecoder(bytes.NewBuffer(buf)).Decode(&checkpointState)
	if err != nil {
		fmt.Printf("decode error: %v, tmp: %v\n", err, checkpointState)
		return Checkpoint{}, err
	}
	checkpoint.CheckpointState = checkpointState

	fmt.Printf("read ssn: %v, state: %v\n", checkpoint.SuperStepNumber, checkpoint.CheckpointState)

	return checkpoint, nil
}

func (w *Worker) RevertToLastCheckpoint(req CheckpointMsg, reply *Checkpoint) error {
	checkpoint, err := w.retrieveCheckpoint(req.SuperStepNumber)
	if err != nil {
		fmt.Printf("error retrieving checkpoint: %v\n", err)
		return err
	}
	fmt.Printf("retrieved checkpoint: %v\n", checkpoint)

	w.SuperStep.Id = checkpoint.SuperStepNumber
	for k, v := range w.Vertices {
		if state, found := checkpoint.CheckpointState[v.Id]; found {
			fmt.Printf("found state: %v\n", state)
			v.currentValue = state.CurrentValue
			v.isActive = state.IsActive
			v.messages = state.Messages
			w.Vertices[k] = v
		}
	}
	// TODO: call compute wrapper with new superstep #
	/*
		@author Ryan:
			1) Coord -> Workers to recover to checkpoint S
			2) Workers respond (ie. all recovered checkpoint)
			3) Coord -> Workers proceed to Computer SS #(S + 1)
	*/
	*reply = checkpoint
	return nil
}

func (w *Worker) listenCoord(handler *rpc.Server) {
	listenAddr, err := net.ResolveTCPAddr("tcp", w.config.WorkerListenAddr)
	util.CheckErr(err, fmt.Sprintf("Worker %v could not resolve WorkerListenAddr: %v", w.config.WorkerId,
		w.config.WorkerListenAddr))
	listen, err := net.ListenTCP("tcp", listenAddr)
	util.CheckErr(err, fmt.Sprintf("Worker %v could not listen on listenAddr: %v", w.config.WorkerId, listenAddr))

	for {
		conn, err := listen.Accept()
		util.CheckErr(err, fmt.Sprintf("Worker %v could not accept connections\n", w.config.WorkerId))
		go handler.ServeConn(conn)
	}
}

// create a new RPC Worker instance for the current Worker
/*
func (w *Worker) register() {
	handler := rpc.NewServer()
	err := handler.Register(w)
	fmt.Printf("register: Worker %v - register error: %v\n", w.WorkerId, err)

	go w.listenCoord(handler)
}
*/

func (w *Worker) Start() error {
	// set Worker state
	if w.config.WorkerAddr == "" {
		return errors.New("Failed to start worker. Please initialize worker before calling Start")
	}

	// connect to the coord node
	conn, err := util.DialTCPCustom(w.config.WorkerListenAddr, w.config.CoordAddr)
	util.CheckErr(err, fmt.Sprintf("Worker %d failed to Dial Coordinator - %s\n", w.config.WorkerId, w.config.CoordAddr))

	defer conn.Close()
	coordClient := rpc.NewClient(conn)

	hBeatAddr := w.startFCheckHBeat(w.config.WorkerId, w.config.FCheckAckLocalAddress)
	fmt.Printf("hBeatAddr for Worker %d is %v\n", w.config.WorkerId, hBeatAddr)

	workerNode := WorkerNode{w.config.WorkerId, w.config.WorkerAddr, hBeatAddr}

	var response WorkerNode
	err = coordClient.Call("Coord.JoinWorker", workerNode, &response)
	util.CheckErr(err, fmt.Sprintf("Worker %v could not join\n", w.config.WorkerId))

	// register Worker for RPC
	// w.register()

	fmt.Printf("Worker: Start: worker %v joined to coord successfully\n", w.config.WorkerId)

	wg := sync.WaitGroup{}
	wg.Add(1)

	// go wait for work to do
	return nil
}

/*
	// test sqlite
	w.partition = []Vertex{
		{
			neighbors:    nil,
			currentValue: 5,
			messages: []Message{{
				Value: 10, SourceVertexId: 2}, {
				Value: 10, SourceVertexId: 2}},
			isActive:   true,
			workerAddr: "test addr",
			vertexId:   1,
		}, {
			neighbors:    nil,
			currentValue: 10,
			messages: []Message{{
				Value: 15, SourceVertexId: 1}, {
				Value: 15, SourceVertexId: 1}},
			isActive:   true,
			workerAddr: "test addr 2",
			vertexId:   2,
		},
	}
	checkpoint := Checkpoint{SuperStepNumber: 0, CheckpointState: make(map[uint64]VertexCheckpoint)}
	for _, v := range w.partition {
		vertexCheckpoint := VertexCheckpoint{
			//VertexId:     v.vertexId,
			CurrentValue: v.currentValue,
			Messages:     v.messages,
			IsActive:     v.isActive,
		}
		//checkpoint.CheckpointState = append(checkpoint.CheckpointState, vertexCheckpoint)
		checkpoint.CheckpointState[v.vertexId] = vertexCheckpoint
	}

	checkpoint, err = w.storeCheckpoint(checkpoint)
	fmt.Printf("stored checkpoint: %v\n", checkpoint)

	checkpoint, err = w.retrieveCheckpoint(0)
	fmt.Printf("retrieved checkpoint: %v\n", checkpoint)

	wg.Wait()

	return nil
}
*/

func (w *Worker) ComputeVertices(args SuperStep, resp *SuperStep) error {

	for _, vertex := range w.Vertices {
		messageMap := vertex.Compute()
		w.updateMessageMap(messageMap)
	}

	if args.IsCheckpoint {
		checkpoint := w.checkpoint()
		w.storeCheckpoint(checkpoint)
	}

	resp = &w.SuperStep
	return nil
}

func (w *Worker) ReceiveMessages(args Message, resp *Message) error {
	if w.SuperStep.Id+1 != args.superStepNum {
		return nil // ignore msgs not for next superstep
	}

	w.NextSuperStep.Messages[args.destVertexId] =
		append(w.NextSuperStep.Messages[args.destVertexId], args)
	resp = &args
	return nil
}

func (w *Worker) handleSuperStepDone() error {
	fmt.Printf("Worker %v transitioning from superstep # %d to superstep # %d\n",
		w.config.WorkerId, w.SuperStep.Id, w.NextSuperStep.Id)
	err := w.sendSuperStepDone()

	if err != nil {
		return err
	}

	w.SuperStep = w.NextSuperStep
	w.NextSuperStep = SuperStep{}
	return nil
}

func (w *Worker) sendSuperStepDone() error {
	raddr, err := net.ResolveTCPAddr("tcp", w.config.CoordAddr)
	util.CheckErr(err, fmt.Sprintf("Failed to resolve coord address %d\n"), w.config.WorkerId)

	conn, err := net.DialTCP("tcp", nil, raddr)
	util.CheckErr(err, fmt.Sprintf("Failed to establish TCP connection with coord: %v\n", w.config.CoordAddr))

	defer conn.Close()

	var resp SuperStepDone

	client := rpc.NewClient(conn)
	// Todo determine coord RPC func method
	err = client.Call("Coord.SuperStepDone", w.SuperStep, &resp)
	return err
}

func (w *Worker) updateMessageMap(msgMap map[uint32]uint64) {
	for worker, numMessages := range msgMap {
		w.SuperStep.Outgoing[worker] += numMessages
	}
}
