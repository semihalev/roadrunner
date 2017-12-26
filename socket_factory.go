package roadrunner

import (
	"encoding/json"
	"fmt"
	"github.com/spiral/goridge"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"
)

// SocketFactory connects to external workers using socket server.
type SocketFactory struct {
	ls   net.Listener                      // listens for incoming connections from underlying processes
	tout time.Duration                     // connection timeout
	mu   sync.Mutex                        // protects socket mapping
	wait map[int]chan *goridge.SocketRelay // sockets which are waiting for process association
}

// NewSocketFactory returns SocketFactory attached to a given socket listener. tout specifies for how long factory
// should wait for incoming relay connection
func NewSocketFactory(ls net.Listener, tout time.Duration) *SocketFactory {
	f := &SocketFactory{
		ls:   ls,
		tout: tout,
		wait: make(map[int]chan *goridge.SocketRelay),
	}

	go f.listen()
	return f
}

// NewWorker creates worker and connects it to appropriate relay or returns error
func (f *SocketFactory) NewWorker(cmd *exec.Cmd) (w *Worker, err error) {
	w, err = NewWorker(cmd)
	if err != nil {
		return nil, err
	}

	if err := w.Start(); err != nil {
		return nil, err
	}

	w.Pid = &w.cmd.Process.Pid
	if w.Pid == nil {
		return nil, fmt.Errorf("can't to start worker %s", w)
	}

	rl, err := f.waitRelay(*w.Pid, f.tout)
	if err != nil {
		return nil, fmt.Errorf("can't connect to worker %s: %s", w, err)
	}

	w.attach(rl)

	return w, nil
}

// Close closes all open factory descriptors.
func (f *SocketFactory) Close() error {
	return f.ls.Close()
}

// listen for incoming wait and associate sockets with active workers
func (f *SocketFactory) listen() {
	for {
		conn, err := f.ls.Accept()
		if err != nil {
			return
		}

		rl := goridge.NewSocketRelay(conn)
		if pid, err := fetchPID(rl); err == nil {
			f.relayChan(pid) <- rl
		}
	}
}

// waits for worker to connect over socket and returns associated relay of timeout
func (f *SocketFactory) waitRelay(pid int, tout time.Duration) (*goridge.SocketRelay, error) {
	timer := time.NewTimer(tout)
	select {
	case rl := <-f.relayChan(pid):
		timer.Stop()
		f.cleanChan(pid)

		return rl, nil
	case <-timer.C:
		return nil, fmt.Errorf("relay timer for [%v]", pid)
	}
}

// chan to store relay associated with specific Pid
func (f *SocketFactory) relayChan(pid int) chan *goridge.SocketRelay {
	f.mu.Lock()
	defer f.mu.Unlock()

	rl, ok := f.wait[pid]
	if !ok {
		f.wait[pid] = make(chan *goridge.SocketRelay)
		return f.wait[pid]
	}

	return rl
}

// deletes relay chan associated with specific Pid
func (f *SocketFactory) cleanChan(pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.wait, pid)
}

// send control command to relay and return associated Pid (or error)
func fetchPID(rl goridge.Relay) (pid int, err error) {
	if err := sendCommand(rl, PidCommand{Pid: os.Getpid()}); err != nil {
		return 0, err
	}

	body, p, err := rl.Receive()
	if !p.HasFlag(goridge.PayloadControl) {
		return 0, fmt.Errorf("unexpected response, `control` header is missing")
	}

	link := &PidCommand{}
	if err := json.Unmarshal(body, link); err != nil {
		return 0, err
	}

	if link.Parent != os.Getpid() {
		return 0, fmt.Errorf("integrity error, parent process does not match")
	}

	return link.Pid, nil
}