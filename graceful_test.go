package graceful

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"
)

var killTime = 50 * time.Millisecond

func runQuery(t *testing.T, expected int, shouldErr bool, wg *sync.WaitGroup) {
	wg.Add(1)
	defer wg.Done()
	client := http.Client{}
	r, err := client.Get("http://localhost:3000")
	if shouldErr && err == nil {
		t.Fatal("Expected an error but none was encountered.")
	} else if shouldErr && err != nil {
		if err.(*url.Error).Err == io.EOF {
			return
		}
		errno := err.(*url.Error).Err.(*net.OpError).Err.(syscall.Errno)
		if errno == syscall.ECONNREFUSED {
			return
		} else if err != nil {
			t.Fatal("Error on Get:", err)
		}
	}

	if r != nil && r.StatusCode != expected {
		t.Fatalf("Incorrect status code on response. Expected %d. Got %d", expected, r.StatusCode)
	} else if r == nil {
		t.Fatal("No response when a response was expected.")
	}
}

func createListener(sleep time.Duration) (*http.Server, net.Listener, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		time.Sleep(sleep)
		rw.WriteHeader(http.StatusOK)
	})

	server := &http.Server{Addr: ":3000", Handler: mux}
	l, err := net.Listen("tcp", ":3000")
	return server, l, err
}

func runServer(timeout, sleep time.Duration, c chan os.Signal, c2 chan struct{}) error {
	server, l, err := createListener(sleep)
	if err != nil {
		return err
	}

	srv := &Server{Timeout: timeout, Server: server, Signal: c, Cancel: c2}
	return srv.Serve(l)
}

func launchTestQueries(t *testing.T, wg *sync.WaitGroup, c chan os.Signal, c2 chan struct{}) {
	for i := 0; i < 8; i++ {
		go runQuery(t, http.StatusOK, false, wg)
	}

	time.Sleep(10 * time.Millisecond)
	if c != nil {
		c <- os.Interrupt
	}
	if c2 != nil {
		close(c2)
	}
	time.Sleep(10 * time.Millisecond)

	for i := 0; i < 8; i++ {
		go runQuery(t, 0, true, wg)
	}

	wg.Done()
}

func TestGracefulRun(t *testing.T) {
	c := make(chan os.Signal, 1)

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		runServer(killTime, killTime/2, c, nil)
		wg.Done()
	}()

	wg.Add(1)
	go launchTestQueries(t, &wg, c, nil)
	wg.Wait()
}

func TestGracefulRunWithCustomChannel(t *testing.T) {
	c := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		runServer(killTime, killTime/2, nil, c)
		wg.Done()
	}()

	wg.Add(1)
	go launchTestQueries(t, &wg, nil, c)
	wg.Wait()
}

func TestGracefulRunWithBothChannels(t *testing.T) {
	c := make(chan os.Signal, 1)
	c2 := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		runServer(killTime, killTime/2, c, c2)
		wg.Done()
	}()

	wg.Add(1)
	go launchTestQueries(t, &wg, c, c2)
	wg.Wait()
}

func TestGracefulRunTimesOut(t *testing.T) {
	c := make(chan os.Signal, 1)

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		runServer(killTime, killTime*10, c, nil)
		wg.Done()
	}()

	wg.Add(1)
	go func() {
		for i := 0; i < 8; i++ {
			go runQuery(t, 0, true, &wg)
		}
		time.Sleep(10 * time.Millisecond)
		c <- os.Interrupt
		time.Sleep(10 * time.Millisecond)
		for i := 0; i < 8; i++ {
			go runQuery(t, 0, true, &wg)
		}
		wg.Done()
	}()

	wg.Wait()

}

func TestGracefulRunDoesntTimeOut(t *testing.T) {
	c := make(chan os.Signal, 1)

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		runServer(0, killTime*2, c, nil)
		wg.Done()
	}()

	wg.Add(1)
	go launchTestQueries(t, &wg, c, nil)
	wg.Wait()
}

func TestGracefulRunNoRequests(t *testing.T) {
	c := make(chan os.Signal, 1)

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		runServer(0, killTime*2, c, nil)
		wg.Done()
	}()

	c <- os.Interrupt

	wg.Wait()

}

func TestGracefulForwardsConnState(t *testing.T) {
	c := make(chan os.Signal, 1)
	states := make(map[http.ConnState]int)

	connState := func(conn net.Conn, state http.ConnState) {
		states[state]++
	}

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		server, l, _ := createListener(killTime / 2)
		srv := &Server{
			ConnState: connState,
			Timeout:   killTime,
			Server:    server,
			Signal:    c,
		}
		srv.Serve(l)

		wg.Done()
	}()

	wg.Add(1)
	go launchTestQueries(t, &wg, c, nil)
	wg.Wait()

	expected := map[http.ConnState]int{
		http.StateNew:    8,
		http.StateActive: 8,
		http.StateClosed: 8,
	}

	if !reflect.DeepEqual(states, expected) {
		t.Errorf("Incorrect connection state tracking.\n  actual: %v\nexpected: %v\n", states, expected)
	}
}
