package httpconnect

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/golog"
	"github.com/getlantern/idletiming"
	"github.com/getlantern/ops"

	"github.com/getlantern/http-proxy/buffers"
	"github.com/getlantern/http-proxy/utils"
)

var log = golog.LoggerFor("httpconnect")

type HTTPConnectHandler struct {
	next         http.Handler
	idleTimeout  time.Duration
	allowedPorts []int
}

type optSetter func(f *HTTPConnectHandler) error

func IdleTimeoutSetter(i time.Duration) optSetter {
	return func(f *HTTPConnectHandler) error {
		f.idleTimeout = i
		return nil
	}
}

func AllowedPorts(ports []int) optSetter {
	return func(f *HTTPConnectHandler) error {
		f.allowedPorts = ports
		return nil
	}
}

func AllowedPortsFromCSV(csv string) optSetter {
	return func(f *HTTPConnectHandler) error {
		fields := strings.Split(csv, ",")
		ports := make([]int, len(fields))
		for i, f := range fields {
			p, err := strconv.Atoi(f)
			if err != nil {
				return err
			}
			ports[i] = p
		}
		f.allowedPorts = ports
		return nil
	}
}

func New(next http.Handler, setters ...optSetter) (*HTTPConnectHandler, error) {
	if next == nil {
		return nil, errors.New("Next handler is not defined (nil)")
	}
	f := &HTTPConnectHandler{next: next}
	for _, s := range setters {
		if err := s(f); err != nil {
			return nil, err
		}
	}

	return f, nil
}

func (f *HTTPConnectHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != "CONNECT" {
		f.next.ServeHTTP(w, req)
		return
	}

	if log.IsTraceEnabled() {
		reqStr, _ := httputil.DumpRequest(req, true)
		log.Tracef("HTTPConnectHandler Middleware received request:\n%s", reqStr)
	}

	op := ops.Enter("proxy_https")
	defer op.Exit()
	if f.portAllowed(op, w, req) {
		f.intercept(op, w, req)
	}
}

func (f *HTTPConnectHandler) portAllowed(op ops.Op, w http.ResponseWriter, req *http.Request) bool {
	if len(f.allowedPorts) == 0 {
		return true
	}
	log.Tracef("Checking CONNECT tunnel to %s against allowed ports %v", req.Host, f.allowedPorts)
	_, portString, err := net.SplitHostPort(req.Host)
	if err != nil {
		// CONNECT request should always include port in req.Host.
		// Ref https://tools.ietf.org/html/rfc2817#section-5.2.
		f.ServeError(op, w, req, http.StatusBadRequest, "No port field in Request-URI / Host header")
		return false
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		f.ServeError(op, w, req, http.StatusBadRequest, "Invalid port")
		return false
	}

	for _, p := range f.allowedPorts {
		if port == p {
			return true
		}
	}
	f.ServeError(op, w, req, http.StatusForbidden, "Port not allowed")
	return false
}

func (f *HTTPConnectHandler) intercept(op ops.Op, w http.ResponseWriter, req *http.Request) (err error) {
	utils.RespondOK(w, req)

	clientConn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		desc := op.Errorf("Unable to hijack connection: %s", err)
		utils.RespondBadGateway(w, req, desc)
		return
	}
	connOutRaw, err := net.DialTimeout("tcp", req.Host, 10*time.Second)
	if err != nil {
		op.Errorf("Unable to dial %v: %v", req.Host, err)
		return
	}
	connOut := idletiming.Conn(connOutRaw, f.idleTimeout, func() {
		if connOutRaw != nil {
			connOutRaw.Close()
		}
	})

	// Pipe data through CONNECT tunnel
	closeConns := func() {
		if clientConn != nil {
			if err := clientConn.Close(); err != nil {
				log.Debugf("Error closing the out connection: %s", err)
			}
		}
		if connOut != nil {
			if err := connOut.Close(); err != nil {
				log.Debugf("Error closing the client connection: %s", err)
			}
		}
	}

	var readFinished sync.WaitGroup
	readFinished.Add(1)
	op.Go(func() {
		buf := buffers.Get()
		defer buffers.Put(buf)
		_, readErr := io.CopyBuffer(connOut, clientConn, buf)
		if readErr != nil {
			log.Debug(op.Errorf("Unable to read from origin: %v", readErr))
		}
		readFinished.Done()
	})

	buf := buffers.Get()
	defer buffers.Put(buf)
	_, writeErr := io.CopyBuffer(clientConn, connOut, buf)
	if writeErr != nil {
		log.Debug(op.Errorf("Unable to write to origin: %v", writeErr))
	}
	readFinished.Wait()
	closeConns()

	return
}

func (f *HTTPConnectHandler) ServeError(op ops.Op, w http.ResponseWriter, req *http.Request, statusCode int, reason interface{}) {
	log.Error(op.Errorf("Respond error to CONNECT request to %s: %d %v", req.Host, statusCode, reason))
	w.WriteHeader(statusCode)
	fmt.Fprintf(w, "%v", reason)
}
