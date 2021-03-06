// mapred is a mapreduce implmentation for fast, scalable and efficient distributed systems in go
package mapred

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/rpc"
	"os"
	"path"
	"strconv"
)

// State of a worker
type state int

// State of a host
const (
	idle state = iota
	inProgress
	completed
	failed
)

// Default chunk size is 16MB
const defaultChunkSize int64 = 1 << 14

// Renaming the generic type - id from Objective-C.
// This is only for less typing - lazy me!
type Id interface{}

// Give input as a map instead of individual key-value pairs to reduce the overhead of function calls
// and accomodate as many key-value pairs as possible in a single function call. How many? That's left
// to the user to decide.
type MapInput map[Id]Id

// Result of the mapreduce result - same as MapInput
type MapReduceResult MapInput

// Intermediate key-value pairs that are emitted
type Intermediate struct {
	Key, Value Id
}

// We use a channel to be able to iterate over the values as and when they are available. This allows
// very large lists to be handled since only as many items are sent through the channel as can fit in memory.
type ReduceInput map[Id]chan Id

// The mapreduce specification object
type Specs struct {
	// A slice of input and output file paths
	InputFiles, OutputFiles []string
	// Total number of mappers and reducers to use
	M, R int
	// Size of each split.
	// This should determined on the basis of the type of the underlying filesystem.
	ChunkSize int64
	// The length of this slice should be >= M+R
	Workers []worker
}

// Represents a machine in the cluster
type worker struct {
	// ID of the worker
	Id wID
	// The IP address of the host
	Addr *net.TCPAddr
	// Task assigned to the worker
	task Id
	// RPC cient for communication
	client *rpc.Client
	// Answers to queries
	ans map[Id]Id // TODO type needs to be changed later
}

// Task consists of the current state of the task, the split assigned to it if it is a map task
type MapTask struct {
	// State of the task
	state state
	// Split assigned to this task - valid only if it is a map task
	split string
	// Same as R, used by map workers
	r int
}

type ReduceTask struct {
	// State of the task
	state state
}

// Type used for providin services over rpc
type Service struct{}

// The MapReduce() call that triggers off the magic
func MapReduce(specs Specs) (result MapReduceResult, err error) {
	// Validate the specs
	if err = validateSpecs(specs); err != nil {
		return
	}

	// Setup rpc services of the master
	setUpMasterServices()

	// Determine the total size of the all input files and start splitting it. Assign each split to a worker as a map task
	// and assign a reduce task to the rest of the workers in a ratio of M:R
	totalWorkers := len(specs.Workers)                      // total workers
	clients := make([]*rpc.Client, totalWorkers)            // clients returned by rpc calls
	splits, err := split(specs.ChunkSize, specs.InputFiles) // Splits of files
	calls := make([]*rpc.Call, totalWorkers)                // calls returned by rpc calls
	unit := totalWorkers / (specs.M + specs.R)              // unit worker for ratios
	ans := make([]bool, totalWorkers)                       // answers received from machines wether they are idle or not?
	m := int(unit * specs.M)                                // number of map workers
	r := totalWorkers - m                                   // number of reduce workers
	dialFailures := 0                                       // number of dial failure - max limit is
	if err != nil {
		return
	}
	myAddr, err := myTCPAddr()
	if err != nil {
		return
	}

	//Ask all hosts wether they are or idle or not
	for i := 0; i < m+r; i++ {
		clients[i], err = rpc.DialHTTP("tcp", specs.Workers[i].Addr.String())
		if err != nil {
			dialFailures++
			if dialFailures > totalWorkers/2 {
				err = fmt.Errorf("Number of dial failures is more than %d", totalWorkers/2)
				return // Return if number of dial failures are too many
			}
		}
		calls[i] = clients[i].Go(idleService, *myAddr, &ans[i], nil) // Call the service method to ask if the host is idle
	}

	// Accept the first m map workers which reply yes
	done := make([]bool, m)
	signalCompletion := make(chan bool, 2) // to signal completion of accept for m & r
	// Pointers to map and reduce workers to get faster access to them
	mapWorkers := make([]*worker, m)
	reduceWorkers := make([]*worker, r)
	// A function accept te first n workers. Value of n is used to check if its for mappers or reducers
	accept := func(n int) {
		i, aw := 0, 0 // aw => Accepted Workers
		// loop unitl we have covered all clients or got m accepted workers
		for ; aw < n; i = (i + 1) % totalWorkers {
			select {
			case <-calls[i].Done:
				if done[i] {
					break
				}
				// Assign the task
				switch n {
				case m:
					specs.Workers[i].task = MapTask{idle, splits[aw], specs.R} // Setup the worker
					mapWorkers[aw] = &specs.Workers[i]                         // Assign reference to mapWorkers
				case r:
					specs.Workers[i].task = ReduceTask{idle}
					reduceWorkers[aw] = &specs.Workers[i]
				}
				specs.Workers[i].client = clients[i] // Assign the client
				done[i] = true                       // mark caller as accepted
				aw++
			default:
				continue
			}
		}
		if aw == n {
			signalCompletion <- true
		} else {
			signalCompletion <- false
		}
	}
	go accept(m)
	go accept(r)
	if <-signalCompletion && <-signalCompletion {
		err = fmt.Errorf("Could not gather enough hosts to work. M: %d, R: %d", m, r)
		return
	}

	// keep looping until all are done
	for {
		// Ask each of them to perform their duty
		for _, mw := range mapWorkers {
			if mw.task.(MapTask).state == idle {
				mw.client.Go(MapService, mw.task, nil, nil) // Ask to do the map work
			}

		}
		for _, rw := range reduceWorkers {
			if rw.task.(ReduceTask).state == idle {
				rw.client.Go(ReduceService, rw.task, nil, nil) // Ask to do the map work
			}
		}

		// wait for a reply from map workers
	}

	err = nil // Reset err
	return
}

func validateSpecs(specs Specs) error {
	// If []workers is set then M+R >= num of workers
	if specs.Workers != nil && specs.M+specs.R < len(specs.Workers) {
		return fmt.Errorf("M: %d & R: %d are less than the num of hosts: %d", specs.M, specs.R, len(specs.Workers))
	}
	// If the chunk size specified is less than 16Mb then set it to 16Mb
	if specs.ChunkSize < defaultChunkSize {
		specs.ChunkSize = defaultChunkSize
	}
	return nil
}

// Singals a task completed
func (s *Service) TaskCompleted() {

}

func (s *Service) IAmAlive() {

}

func setUpMasterServices() error {
	s := &Service{}
	rpc.Register(s)
	rpc.HandleHTTP()
	var e error
	l, e = net.Listen("tcp", port)
	if e != nil {
		return e
	}
	go http.Serve(l, nil)
	return nil
}

// Split files into chunks of size chunkS and return the file handles
func split(chunkS int64, files []string) ([]string, error) {
	// name each intermediate file based on the hash of the contents of the input file
	var splits []string
	tmp := path.Join(os.TempDir(), "github.com", "ujjwalt", "hagoop")
	b := make([]byte, chunkS)
	var off, read int64 = 0, 0                     // offset and how many bytes read
	for i, si, n := 0, 0, len(files); i < n; i++ { // si is the split index
		fHandle, err := os.Open(files[i])
		if err != nil {
			return splits, err
		}
		defer fHandle.Close()
		// Read chunkS bytes into splits[i]
	tryAgain:
		r, err := fHandle.ReadAt(b[read:chunkS], off) // Read chunkS bytes into b
		off += int64(r)
		read += int64(r)
		switch {
		case read < chunkS:
			if err == io.EOF {
				off = 0 // if the file is over, get ready for the next file
				continue
			} else {
				goto tryAgain
			}
		case read > chunkS:
			return splits, err // something really shitty happened here!
		}
		read = 0 // reset for reading the next chunk
		// Create a temporary file for the split
		splits = append(splits, path.Join(tmp, "split"+strconv.Itoa(si)))
		splitN, err := os.Create(splits[si])
		if err != nil {
			return splits, err
		} else {
			defer splitN.Close()
		}
		splitN.WriteAt(b, off)
		err = splitN.Close()
		if err != nil {
			return splits, err
		}
		si++ // move onto the next split
	}
	return splits, nil
}

// Converts a bool to int
func bToi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func myTCPAddr() (a *net.TCPAddr, err error) {
	// Return own ip address used for the connections by dialing to http://www.google.com
	c, err := net.Dial("tcp", "google.com:80")
	defer c.Close()
	if err != nil {
		return
	}
	host, p, err := net.SplitHostPort(c.LocalAddr().String())
	if err != nil {
		return
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("Invalid ip: %v", ip)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return
	}
	a = &net.TCPAddr{IP: ip, Port: port}
	return
}
