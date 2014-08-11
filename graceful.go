package graceful

import (
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Server wraps an http.Server with graceful connection handling.
// It may be used directly in the same way as http.Server, or may
// be constructed with the global functions in this package.
//
// Example:
//	srv := &graceful.Server{
//		Timeout: 5 * time.Second,
//		Server: &http.Server{Addr: ":1234", Handler: handler},
//	}
//	srv.ListenAndServe()
type Server struct {
	// Timeout is the duration to allow outstanding requests to survive
	// before forcefully terminating them.
	Timeout time.Duration

	// ConnState specifies an optional callback function that is
	// called when a client connection changes state. This is a proxy
	// to the underlying http.Server's ConnState, and the original
	// must not be set directly.
	ConnState func(net.Conn, http.ConnState)

	// interrupt signals the listener to stop serving connections,
	// and the server to shut down.
	Signal chan os.Signal

	// interrupts the listener to stop serving connections,
	// and the server to shut down.
	Cancel chan struct{}

	*http.Server
}

// Run serves the http.Handler with graceful shutdown enabled.
//
// timeout is the duration to wait until killing active requests and stopping the server.
// If timeout is 0, the server never times out. It waits for all active requests to finish.
func Run(addr string, timeout time.Duration, n http.Handler) {
	srv := &Server{
		Timeout: timeout,
		Server:  &http.Server{Addr: addr, Handler: n},
	}

	if err := srv.ListenAndServe(); err != nil {
		if opErr, ok := err.(*net.OpError); !ok || (ok && opErr.Op != "accept") {
			logger := log.New(os.Stdout, "[graceful] ", 0)
			logger.Fatal(err)
		}
	}
}

// ListenAndServe is equivalent to http.Server.ListenAndServe with graceful shutdown enabled.
//
// timeout is the duration to wait until killing active requests and stopping the server.
// If timeout is 0, the server never times out. It waits for all active requests to finish.
func ListenAndServe(server *http.Server, timeout time.Duration) error {
	srv := &Server{Timeout: timeout, Server: server}
	return srv.ListenAndServe()
}

// ListenAndServe is equivalent to http.Server.ListenAndServe with graceful shutdown enabled.
func (srv *Server) ListenAndServe() error {
	// Create the listener so we can control their lifetime
	addr := srv.Addr
	if addr == "" {
		addr = ":http"
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	return srv.Serve(l)
}

// ListenAndServeTLS is equivalent to http.Server.ListenAndServeTLS with graceful shutdown enabled.
//
// timeout is the duration to wait until killing active requests and stopping the server.
// If timeout is 0, the server never times out. It waits for all active requests to finish.
func ListenAndServeTLS(server *http.Server, certFile, keyFile string, timeout time.Duration) error {
	// Create the listener ourselves so we can control its lifetime
	srv := &Server{Timeout: timeout, Server: server}
	addr := srv.Addr
	if addr == "" {
		addr = ":https"
	}

	config := &tls.Config{}
	if srv.TLSConfig != nil {
		*config = *srv.TLSConfig
	}
	if config.NextProtos == nil {
		config.NextProtos = []string{"http/1.1"}
	}

	var err error
	config.Certificates = make([]tls.Certificate, 1)
	config.Certificates[0], err = tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}

	conn, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	tlsListener := tls.NewListener(conn, config)
	return srv.Serve(tlsListener)
}

// Serve is equivalent to http.Server.Serve with graceful shutdown enabled.
//
// timeout is the duration to wait until killing active requests and stopping the server.
// If timeout is 0, the server never times out. It waits for all active requests to finish.
func Serve(server *http.Server, l net.Listener, timeout time.Duration) error {
	srv := &Server{Timeout: timeout, Server: server}
	return srv.Serve(l)
}

// Serve is equivalent to http.Server.Serve with graceful shutdown enabled.
func (srv *Server) Serve(listener net.Listener) error {
	// Track connection state
	add := make(chan net.Conn)
	remove := make(chan net.Conn)

	srv.Server.ConnState = func(conn net.Conn, state http.ConnState) {
		switch state {
		case http.StateActive:
			add <- conn
		case http.StateClosed, http.StateIdle:
			remove <- conn
		}

		if hook := srv.ConnState; hook != nil {
			hook(conn, state)
		}
	}

	// Manage open connections
	stop := make(chan chan struct{})
	kill := make(chan struct{})
	go func() {
		var done chan struct{}
		connections := map[net.Conn]struct{}{}
		for {
			select {
			case conn := <-add:
				connections[conn] = struct{}{}
			case conn := <-remove:
				delete(connections, conn)
				if done != nil && len(connections) == 0 {
					done <- struct{}{}
					return
				}
			case done = <-stop:
				if len(connections) == 0 {
					done <- struct{}{}
					return
				}
			case <-kill:
				for k := range connections {
					k.Close()
				}
				return
			}
		}
	}()

	sig := srv.Signal
	cancel := srv.Cancel

	// If no interrupt is defined, set up a standard signal interrupt
	if sig == nil && cancel == nil {
		sig = make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	}

	stopListener := make(chan struct{})

	go func() {
		select {
		case <-stopListener:
		case <-sig:
		case <-cancel:
		}

		if sig != nil {
			signal.Stop(sig)
			close(sig)
		}
		srv.SetKeepAlivesEnabled(false)
		listener.Close()
	}()

	// Serve with graceful listener
	err := srv.Server.Serve(listener)

	// if Serve quits due to a non-cancellation signal (eg., binding to the same
	// port simultaneously, etc.) the above go routine should be stopped.
	close(stopListener)

	// Request done notification
	done := make(chan struct{})
	stop <- done

	if srv.Timeout > 0 {
		select {
		case <-done:
		case <-time.After(srv.Timeout):
			kill <- struct{}{}
		}
	} else {
		<-done
	}
	return err
}
