package route

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rgumi/depoy/conditional"
	"github.com/rgumi/depoy/metrics"
	"github.com/rgumi/depoy/upstreamclient"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

var (
	ServerName = "depoy/0.1.0"
)

// UpstreamClient is an interface for the http.Client
// it implements the method Send which is used to send http requests
type UpstreamClient interface {
	Send(*http.Request, metrics.Metrics) (*http.Response, metrics.Metrics, error)
}

type Route struct {
	Name                string
	Prefix              string
	Methods             []string
	Host                string
	Rewrite             string
	CookieTTL           time.Duration
	Strategy            *Strategy
	HealthCheck         bool
	HealthCheckInterval time.Duration
	MonitoringInterval  time.Duration
	Timeout             time.Duration
	IdleTimeout         time.Duration
	ScrapeInterval      time.Duration
	Proxy               string
	Backends            map[uuid.UUID]*Backend
	Switchover          *Switchover
	Client              UpstreamClient
	MetricsRepo         *metrics.Repository
	NextTargetDistr     []*Backend
	lenNextTargetDistr  int
	killHealthCheck     chan int
	mux                 sync.RWMutex
}

// New creates a new route-object with the provided config
func New(
	name, prefix, rewrite, host, proxy string,
	methods []string,
	timeout, idleTimeout, scrapeInterval, healthcheckInterval,
	monitoringInterval, cookieTTL time.Duration,
	doHealthCheck bool,
) (*Route, error) {

	client := upstreamclient.NewClient(
		upstreamclient.MaxIdleConns, upstreamclient.MaxIdleConnsPerHost,
		timeout, idleTimeout, proxy, upstreamclient.TLSVerfiy,
	)

	// fix prefix if prefix does not end with /
	if prefix[len(prefix)-1] != '/' {
		prefix += "/"
	}
	route := &Route{
		Name:                name,
		Prefix:              prefix,
		Rewrite:             rewrite,
		Methods:             methods,
		Host:                host,
		Proxy:               proxy,
		Timeout:             timeout,
		IdleTimeout:         idleTimeout,
		ScrapeInterval:      scrapeInterval,
		HealthCheck:         doHealthCheck,
		HealthCheckInterval: healthcheckInterval,
		MonitoringInterval:  monitoringInterval,
		Strategy:            nil,
		Backends:            make(map[uuid.UUID]*Backend),
		killHealthCheck:     make(chan int, 1),
		CookieTTL:           cookieTTL,
		Client:              client,
	}

	if route.HealthCheck {
		go route.RunHealthCheckOnBackends()
	}

	return route, nil
}

func (r *Route) SetStrategy(strategy *Strategy) {
	r.Strategy = strategy
}

func (r *Route) GetHandler() http.HandlerFunc {
	if r.Strategy == nil {
		panic(fmt.Errorf("No strategy is set for %s", r.Name))
	}

	return r.Strategy.Handler
}

func (r *Route) updateWeights() {
	r.mux.Lock()
	defer r.mux.Unlock()

	var sum uint8
	k, i := 0, 0
	listWeights := make([]uint8, len(r.Backends))
	activeBackends := []*Backend{}

	for _, backend := range r.Backends {
		if backend.Active {
			listWeights[i] = backend.Weigth
			activeBackends = append(activeBackends, backend)
			i++
		}
	}
	// find ggt to reduce list length
	ggt := GGT(listWeights) // if 0, return 0
	log.Debugf("Current GGT of Weights is %d", ggt)

	if ggt > 0 {
		for _, weight := range listWeights {
			sum += weight / ggt
		}
		distr := make([]*Backend, sum)

		for _, backend := range activeBackends {
			for i := uint8(0); i < backend.Weigth/ggt; i++ {
				distr[k] = backend
				k++
			}
		}
		log.Debugf("Current TargetDistribution of %s: %v", r.Name, distr)
		r.NextTargetDistr = distr
	} else {
		// no active backend
		r.NextTargetDistr = make([]*Backend, 0)
	}
	r.lenNextTargetDistr = len(r.NextTargetDistr)
}

func (r *Route) getNextBackend() (*Backend, error) {

	if r.lenNextTargetDistr == 0 {
		return nil, fmt.Errorf("No backend is active")
	}

	backend := r.NextTargetDistr[rand.Intn(r.lenNextTargetDistr)]
	return backend, nil
}

// Reload is required if the route is changed (reload config).
// when a new backend is registerd reload handles the initial tasks
// like monitoring and healthcheck
func (r *Route) Reload() {
	log.Infof("Reloading %v", r.Name)
	if !r.HealthCheck {
		log.Warnf("Healthcheck of %s is not active", r.Name)
	}
	if r.MetricsRepo == nil {
		panic(fmt.Errorf("MetricsRepo of %s cannot be nil", r.Name))
	}
	for _, backend := range r.Backends {
		if backend.AlertChan == nil {
			if r.HealthCheck {
				mustHaveCondition := conditional.NewCondition(
					"6xxRate", ">", 0, 5*time.Second, 2*time.Second)
				mustHaveCondition.Compile()
				backend.Metricthresholds = append(backend.Metricthresholds, mustHaveCondition)
			}

			log.Infof("Registering %v of %s to MetricsRepository", backend.ID, r.Name)
			backend.AlertChan, _ = r.MetricsRepo.RegisterBackend(
				r.Name, backend.ID, backend.Scrapeurl, backend.Scrapemetrics,
				r.ScrapeInterval, backend.Metricthresholds,
			)
			// start monitoring the registered backend
			go r.MetricsRepo.Monitor(backend.ID, r.MonitoringInterval)
			// starts listening on alertChan
			go backend.Monitor()

		}

		if r.HealthCheck {
			go r.validateStatus(backend)
		} else {
			r.updateWeights()
		}
	}
}

func (r *Route) validateStatus(backend *Backend) {
	log.Debugf("Executing validateStatus on %v", backend.ID)
	if r.healthCheck(backend) {
		log.Debugf("Finished healtcheck of %v successfully", backend.ID)
		backend.UpdateStatus(true)
		return
	}

	// It sometimes is possible that when a new backend is added and while the
	// backend is registered, the upstream application is just starting (Conn refused),
	// the status does not get updated when the upstream application is healthy again as an
	// alarm has not been registered in the MetricsRepo due to an activeFor which can then be
	// resolved
	if r.MetricsRepo != nil {
		r.MetricsRepo.RegisterAlert(backend.ID, "Pending", "6xxRate", 0, 1)
	}

}

// AddBackend adds another backend instance of the route
// A backend could be another version of the upstream application
// which then can be routed to
func (r *Route) AddBackend(
	name string, addr, scrapeURL, healthCheckURL *url.URL,
	scrapeMetrics []string,
	metricsThresholds []*conditional.Condition,
	weight uint8) (uuid.UUID, error) {

	backend, err := NewBackend(
		name, addr, scrapeURL, healthCheckURL, scrapeMetrics, metricsThresholds, weight)
	if err != nil {
		return uuid.UUID{}, err
	}
	backend.updateWeigth = r.updateWeights

	if r.HealthCheck {
		backend.Active = false
	} else {
		backend.Active = true
	}

	for _, backend := range r.Backends {
		if backend.Name == name {
			return uuid.UUID{}, fmt.Errorf("Backend with given name already exists")
		}
	}

	log.Warnf("Added Backend %v to Route %s", backend.ID, r.Name)
	r.Backends[backend.ID] = backend

	return backend.ID, nil
}

// AddExistingBackend can be used to add an existing backend to a route
func (r *Route) AddExistingBackend(backend *Backend) (uuid.UUID, error) {

	newBackend, err := NewBackend(
		backend.Name, backend.Addr, backend.Scrapeurl, backend.Healthcheckurl, backend.Scrapemetrics,
		backend.Metricthresholds, backend.Weigth,
	)
	if err != nil {
		return uuid.UUID{}, err
	}

	for _, existingBackend := range r.Backends {
		if existingBackend.Name == newBackend.Name {
			return uuid.UUID{}, fmt.Errorf("Backend with given name already exists")
		}
	}

	// status will be set by first healthcheck
	if r.HealthCheck {
		newBackend.Active = false
	} else {
		newBackend.Active = true
	}

	newBackend.updateWeigth = r.updateWeights
	newBackend.ActiveAlerts = make(map[string]metrics.Alert)
	newBackend.killChan = make(chan int, 1)

	log.Warnf("Added Backend %v to Route %s", newBackend.ID, r.Name)
	r.Backends[newBackend.ID] = newBackend
	return newBackend.ID, nil
}

func (r *Route) StopAll() {
	r.killHealthCheck <- 1
	r.RemoveSwitchOver()

	for backendID := range r.Backends {
		r.RemoveBackend(backendID)
	}

}
func (r *Route) RemoveBackend(backendID uuid.UUID) {
	log.Warnf("Removing %s from %s", backendID, r.Name)

	if r.Switchover != nil {
		if r.Switchover.From.ID == backendID || r.Switchover.To.ID == backendID {
			panic(
				fmt.Errorf("Cannot deleted backend %v with switchover %d associated with it",
					backendID, r.Switchover.ID,
				),
			)
		}
	}
	if r.MetricsRepo != nil {
		r.MetricsRepo.RemoveBackend(backendID)
	}

	r.Backends[backendID].Stop()
	delete(r.Backends, backendID)
}

func (r *Route) UpdateBackendWeight(id uuid.UUID, newWeigth uint8) error {
	if backend, found := r.Backends[id]; found {
		backend.Weigth = newWeigth
		r.updateWeights()
		return nil
	}
	return fmt.Errorf("Backend with ID %v does not exist", id)
}

func (r *Route) healthCheck(backend *Backend) bool {
	defer func() {
		if err := recover(); err != nil {
			return
		}
	}()
	req, err := http.NewRequest("GET", backend.Healthcheckurl.String(), nil)
	if err != nil {
		log.Error(err.Error())
		return false
	}
	m := metrics.Metrics{
		Route:          r.Name,
		RequestMethod:  req.Method,
		DownstreamAddr: req.RemoteAddr,
		BackendID:      backend.ID,
	}
	resp, m, err := r.Client.Send(req, m)
	if err != nil {
		log.Debugf("Healthcheck for %v failed due to %v", backend.ID, err)
		if backend.Active {
			backend.UpdateStatus(false)
		}
		m.ResponseStatus = 600
		m.ContentLength = 0
		r.MetricsRepo.InChannel <- m
		return false
	}
	resp.Body.Close()
	m.ResponseStatus = resp.StatusCode
	m.ContentLength = resp.ContentLength
	r.MetricsRepo.InChannel <- m
	return true

}

func (r *Route) RunHealthCheckOnBackends() {
	for {
		select {
		case _ = <-r.killHealthCheck:
			log.Warnf("Stopping healthcheck-loop of %s", r.Name)
			return
		default:
			for _, backend := range r.Backends {
				// could be a go-routine
				go r.healthCheck(backend)
			}
		}
		time.Sleep(r.HealthCheckInterval)
	}

}

// StartSwitchOver starts the switch over process
func (r *Route) StartSwitchOver(
	from, to string,
	conditions []*conditional.Condition,
	timeout time.Duration, allowedFailures int,
	weightChange uint8, force, rollback bool) (*Switchover, error) {

	var fromBackend, toBackend *Backend

	// check if a switchover is already active
	// only one switchover is allowed per route at a time
	if r.Switchover != nil {
		if r.Switchover.Status == "Running" {
			return nil, fmt.Errorf("Only one switchover can be active per route")
		}
	}

	if from == "" {
		// select an existing backend
		for _, backend := range r.Backends {
			if backend.Name != to && backend.Weigth == 100 {
				from = backend.Name
				goto forward
			}
		}
		return nil, fmt.Errorf("from was empty and no backend of route could be selected")
	}

forward:
	for _, backend := range r.Backends {
		if backend.Name == from {
			fromBackend = backend
		} else if backend.Name == to {
			toBackend = backend
		}
	}

	if fromBackend == nil {
		return nil, fmt.Errorf("Cannot find backend with Name %v", from)
	}

	if toBackend == nil {
		return nil, fmt.Errorf("Cannot find backend with Name %v", to)
	}

	if force {
		// Overwrite the current Strategy with StickyStrategy
		strategy, err := NewStickyStrategy(r)
		if err != nil {
			return nil, err
		}
		r.SetStrategy(strategy)

		// set initial weights
		fromBackend.Weigth = 100 - weightChange
		toBackend.Weigth = weightChange

		r.updateWeights()

	} else {
		// The Strategy must be canary (sticky or slippery) because otherwise
		// the traffic cannot be increased/switched-over
		if strings.ToLower(r.Strategy.Type) != "sticky" && strings.ToLower(r.Strategy.Type) != "slippery" {
			return nil, fmt.Errorf(
				"Switchover is only supported with Strategy \"sticky\" or \"slippery\"")
		}
	}

	switchover, err := NewSwitchover(
		fromBackend, toBackend, r, conditions, timeout, allowedFailures, weightChange, rollback)

	if err != nil {
		return nil, err
	}

	r.Switchover = switchover
	go switchover.Start()

	return switchover, nil
}

// RemoveSwitchOver stops the switchover process and leaves the weights as they are last
func (r *Route) RemoveSwitchOver() {
	if r.Switchover != nil {
		log.Warnf("Stopping Switchover of %s", r.Name)
		r.Switchover.Stop()
		r.Switchover = nil
	}
}

func (r *Route) httpDo(
	ctx context.Context,
	target *Backend,
	req *http.Request,
	body io.ReadCloser,
	f func(*http.Response, metrics.Metrics, error) GatewayError) GatewayError {

	c := make(chan error, 1)
	upReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL.String(), body)
	if err != nil {
		return NewGatewayError(err)
	}
	r.setupRequestURL(upReq, target)
	// setup the X-Forwarded-For header
	if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		appendHostToXForwardHeader(upReq.Header, clientIP)
	}
	copyHeaders(req.Header, upReq.Header)
	delHopHeaders(upReq.Header)

	upReq.Close = false
	m := metrics.Metrics{
		BackendID:       target.ID,
		Route:           r.Name,
		DSContentLength: req.ContentLength,
		RequestMethod:   req.Method,
		DownstreamAddr:  req.RemoteAddr,
	}
	go func() { c <- f(r.Client.Send(upReq, m)) }()
	select {
	case <-ctx.Done():
		<-c // Wait for f to return.
		return NewGatewayError(ctx.Err())
	case err := <-c:
		return NewGatewayError(err)
	}
}
func (r *Route) httpReturn(w http.ResponseWriter) func(*http.Response, metrics.Metrics, error) GatewayError {
	return func(resp *http.Response, m metrics.Metrics, err error) GatewayError {
		if err != nil {
			return NewGatewayError(err)
		}
		w.WriteHeader(resp.StatusCode)
		copyHeaders(resp.Header, w.Header())
		w.Header().Add("Server", ServerName)
		n, err := copyBuffer(w, resp.Body, nil)
		if err != nil {
			defer resp.Body.Close()
			return NewGatewayError(err)
		}
		resp.Body.Close()
		m.ResponseStatus = resp.StatusCode
		m.ContentLength = int64(n)
		r.MetricsRepo.InChannel <- m
		return nil
	}
}

func copyBody(dst io.Writer, src io.Reader) error {
	_, err := io.Copy(dst, src)
	return err
}

func (r *Route) setupRequestURL(req *http.Request, backend *Backend) {
	req.URL.Scheme = backend.Addr.Scheme
	req.URL.Host = backend.Addr.Host
	if r.Rewrite != "" {
		req.URL.Path = strings.Replace(req.URL.Path, r.Prefix, r.Rewrite, 1)
	}
}

type writeFlusher interface {
	io.Writer
	http.Flusher
}

type maxLatencyWriter struct {
	dst          writeFlusher
	latency      time.Duration // non-zero; negative means to flush immediately
	mu           sync.Mutex    // protects t, flushPending, and dst.Flush
	t            *time.Timer
	flushPending bool
}

func (m *maxLatencyWriter) Write(p []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err = m.dst.Write(p)
	if m.latency < 0 {
		m.dst.Flush()
		return
	}
	if m.flushPending {
		return
	}
	if m.t == nil {
		m.t = time.AfterFunc(m.latency, m.delayedFlush)
	} else {
		m.t.Reset(m.latency)
	}
	m.flushPending = true
	return
}

func (m *maxLatencyWriter) delayedFlush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.flushPending { // if stop was called but AfterFunc already started this goroutine
		return
	}
	m.dst.Flush()
	m.flushPending = false
}

func (m *maxLatencyWriter) stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushPending = false
	if m.t != nil {
		m.t.Stop()
	}
}

func copyResponse(dst io.Writer, src io.Reader, flushInterval time.Duration) error {
	if flushInterval != 0 {
		if wf, ok := dst.(writeFlusher); ok {
			mlw := &maxLatencyWriter{
				dst:     wf,
				latency: flushInterval,
			}
			defer mlw.stop()

			// set up initial timer so headers get flushed even if body writes are delayed
			mlw.flushPending = true
			mlw.t = time.AfterFunc(flushInterval, mlw.delayedFlush)

			dst = mlw
		}
	}

	var buf []byte
	_, err := copyBuffer(dst, src, buf)
	return err
}

// copyBuffer returns any write errors or non-EOF read errors, and the amount
// of bytes written.
func copyBuffer(dst io.Writer, src io.Reader, buf []byte) (int64, error) {
	if len(buf) == 0 {
		buf = make([]byte, 32*1024)
	}
	var written int64
	for {
		nr, rerr := src.Read(buf)
		if rerr != nil && rerr != io.EOF && rerr != context.Canceled {
			log.Errorf("read error during body copy: %v", rerr)
		}
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if werr != nil {
				return written, werr
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				rerr = nil
			}
			return written, rerr
		}
	}
}
