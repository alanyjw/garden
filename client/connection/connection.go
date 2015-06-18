package connection

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/cloudfoundry-incubator/garden"
	"github.com/cloudfoundry-incubator/garden/routes"
	"github.com/cloudfoundry-incubator/garden/transport"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/rata"
)

var ErrDisconnected = errors.New("disconnected")
var ErrInvalidMessage = errors.New("invalid message payload")

//go:generate counterfeiter . Connection

type Connection interface {
	Ping() error

	Capacity() (garden.Capacity, error)

	Create(spec garden.ContainerSpec) (string, error)
	List(properties garden.Properties) ([]string, error)

	// Destroys the container with the given handle. If the container cannot be
	// found, garden.ContainerNotFoundError is returned. If deletion fails for another
	// reason, another error type is returned.
	Destroy(handle string) error

	Stop(handle string, kill bool) error

	Info(handle string) (garden.ContainerInfo, error)
	BulkInfo(handles []string) (map[string]garden.ContainerInfoEntry, error)
	BulkMetrics(handles []string) (map[string]garden.ContainerMetricsEntry, error)

	StreamIn(handle string, dstPath string, reader io.Reader) error
	StreamOut(handle string, srcPath string) (io.ReadCloser, error)

	LimitBandwidth(handle string, limits garden.BandwidthLimits) (garden.BandwidthLimits, error)
	LimitCPU(handle string, limits garden.CPULimits) (garden.CPULimits, error)
	LimitDisk(handle string, limits garden.DiskLimits) (garden.DiskLimits, error)
	LimitMemory(handle string, limit garden.MemoryLimits) (garden.MemoryLimits, error)

	CurrentBandwidthLimits(handle string) (garden.BandwidthLimits, error)
	CurrentCPULimits(handle string) (garden.CPULimits, error)
	CurrentDiskLimits(handle string) (garden.DiskLimits, error)
	CurrentMemoryLimits(handle string) (garden.MemoryLimits, error)

	Run(handle string, spec garden.ProcessSpec, io garden.ProcessIO) (garden.Process, error)
	Attach(handle string, processID uint32, io garden.ProcessIO) (garden.Process, error)

	NetIn(handle string, hostPort, containerPort uint32) (uint32, uint32, error)
	NetOut(handle string, rule garden.NetOutRule) error

	Properties(handle string) (garden.Properties, error)
	Property(handle string, name string) (string, error)
	SetProperty(handle string, name string, value string) error

	Metrics(handle string) (garden.Metrics, error)
	RemoveProperty(handle string, name string) error
}

//go:generate counterfeiter . Hijacker
type Hijacker interface {
	Hijack(handler string, body io.Reader, params rata.Params, query url.Values, contentType string) (net.Conn, *bufio.Reader, error)
}

type connection struct {
	req *rata.RequestGenerator

	hijacker Hijacker

	noKeepaliveClient *http.Client
	log               lager.Logger
}

type Error struct {
	StatusCode int
	Message    string
}

func (err Error) Error() string {
	return err.Message
}

func New(network, address string) Connection {
	return NewWithLogger(network, address, lager.NewLogger("garden-connection"))
}

func NewWithLogger(network, address string, log lager.Logger) Connection {
	hijacker, dialer := NewHijackerWithDialer(network, address)

	return NewWithHijacker(network, address, dialer, hijacker, log)
}

func NewWithHijacker(network, address string, dialer func(string, string) (net.Conn, error), hijacker Hijacker, log lager.Logger) Connection {
	req := rata.NewRequestGenerator("http://api", routes.Routes)

	return &connection{
		req: req,

		hijacker: hijacker,

		noKeepaliveClient: &http.Client{
			Transport: &http.Transport{
				Dial:              dialer,
				DisableKeepAlives: true,
			},
		},

		log: log,
	}
}

func (c *connection) Ping() error {
	return c.do(routes.Ping, nil, &struct{}{}, nil, nil)
}

func (c *connection) Capacity() (garden.Capacity, error) {
	capacity := garden.Capacity{}
	err := c.do(routes.Capacity, nil, &capacity, nil, nil)
	if err != nil {
		return garden.Capacity{}, err
	}

	return capacity, nil
}

func (c *connection) Create(spec garden.ContainerSpec) (string, error) {
	res := struct {
		Handle string `json:"handle"`
	}{}

	err := c.do(routes.Create, spec, &res, nil, nil)
	if err != nil {
		return "", err
	}

	return res.Handle, nil
}

func (c *connection) Stop(handle string, kill bool) error {
	return c.do(
		routes.Stop,
		map[string]bool{
			"kill": kill,
		},
		&struct{}{},
		rata.Params{
			"handle": handle,
		},
		nil,
	)
}

func (c *connection) Destroy(handle string) error {
	return c.do(
		routes.Destroy,
		nil,
		&struct{}{},
		rata.Params{
			"handle": handle,
		},
		nil,
	)
}

func (c *connection) Run(handle string, spec garden.ProcessSpec, processIO garden.ProcessIO) (garden.Process, error) {
	reqBody := new(bytes.Buffer)

	err := transport.WriteMessage(reqBody, spec)
	if err != nil {
		return nil, err
	}

	hijackedConn, hijackedResponseReader, err := c.hijacker.Hijack(
		routes.Run,
		reqBody,
		rata.Params{
			"handle": handle,
		},
		nil,
		"application/json",
	)
	if err != nil {
		return nil, err
	}

	return c.streamProcess(handle, processIO, hijackedConn, hijackedResponseReader)
}

func (c *connection) Attach(handle string, processID uint32, processIO garden.ProcessIO) (garden.Process, error) {
	reqBody := new(bytes.Buffer)

	hijackedConn, hijackedResponseReader, err := c.hijacker.Hijack(
		routes.Attach,
		reqBody,
		rata.Params{
			"handle": handle,
			"pid":    fmt.Sprintf("%d", processID),
		},
		nil,
		"",
	)
	if err != nil {
		return nil, err
	}

	return c.streamProcess(handle, processIO, hijackedConn, hijackedResponseReader)
}

func (c *connection) streamProcess(handle string, processIO garden.ProcessIO, hijackedConn net.Conn, hijackedResponseReader *bufio.Reader) (garden.Process, error) {
	decoder := json.NewDecoder(hijackedResponseReader)

	payload := &transport.ProcessPayload{}
	if err := decoder.Decode(payload); err != nil {
		return nil, err
	}

	processPipeline := &processStream{
		processID: payload.ProcessID,
		conn:      hijackedConn,
	}

	hijack := func(streamType string) (net.Conn, io.Reader, error) {
		params := rata.Params{
			"handle":   handle,
			"pid":      fmt.Sprintf("%d", processPipeline.ProcessID()),
			"streamid": fmt.Sprintf("%d", payload.StreamID),
		}

		return c.hijacker.Hijack(
			streamType,
			nil,
			params,
			nil,
			"application/json",
		)
	}

	process := newProcess(payload.ProcessID, processPipeline)
	streamHandler := newStreamHandler(processPipeline, c.log)

	streamHandler.streamIn(processIO.Stdin)

	var stdoutConn net.Conn
	if processIO.Stdout != nil {
		var (
			stdout io.Reader
			err    error
		)
		stdoutConn, stdout, err = hijack(routes.Stdout)
		if err != nil {
			werr := fmt.Errorf("connection: failed to hijack stream %s: %s", routes.Stdout, err)
			process.exited(0, werr)
			hijackedConn.Close()
			return process, nil
		}
		streamHandler.streamOut(processIO.Stdout, stdout)
	}

	var stderrConn net.Conn
	if processIO.Stderr != nil {
		var (
			stderr io.Reader
			err    error
		)
		stderrConn, stderr, err = hijack(routes.Stderr)
		if err != nil {
			werr := fmt.Errorf("connection: failed to hijack stream %s: %s", routes.Stderr, err)
			process.exited(0, werr)
			hijackedConn.Close()
			return process, nil
		}
		streamHandler.streamOut(processIO.Stderr, stderr)
	}

	go func() {
		defer hijackedConn.Close()
		if stdoutConn != nil {
			defer stdoutConn.Close()
		}
		if stderrConn != nil {
			defer stderrConn.Close()
		}

		exitCode, err := streamHandler.wait(decoder)
		process.exited(exitCode, err)
	}()

	return process, nil
}

func (c *connection) NetIn(handle string, hostPort, containerPort uint32) (uint32, uint32, error) {
	res := &transport.NetInResponse{}

	err := c.do(
		routes.NetIn,
		&transport.NetInRequest{
			Handle:        handle,
			HostPort:      hostPort,
			ContainerPort: containerPort,
		},
		res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	if err != nil {
		return 0, 0, err
	}

	return res.HostPort, res.ContainerPort, nil
}

func (c *connection) NetOut(handle string, rule garden.NetOutRule) error {
	return c.do(
		routes.NetOut,
		rule,
		&struct{}{},
		rata.Params{
			"handle": handle,
		},
		nil,
	)
}

func (c *connection) Property(handle string, name string) (string, error) {
	var res struct {
		Value string `json:"value"`
	}

	err := c.do(
		routes.Property,
		nil,
		&res,
		rata.Params{
			"handle": handle,
			"key":    name,
		},
		nil,
	)

	return res.Value, err
}

func (c *connection) SetProperty(handle string, name string, value string) error {
	err := c.do(
		routes.SetProperty,
		map[string]string{
			"value": value,
		},
		&struct{}{},
		rata.Params{
			"handle": handle,
			"key":    name,
		},
		nil,
	)

	if err != nil {
		return err
	}

	return nil
}

func (c *connection) RemoveProperty(handle string, name string) error {
	err := c.do(
		routes.RemoveProperty,
		nil,
		&struct{}{},
		rata.Params{
			"handle": handle,
			"key":    name,
		},
		nil,
	)

	if err != nil {
		return err
	}

	return nil
}

func (c *connection) LimitBandwidth(handle string, limits garden.BandwidthLimits) (garden.BandwidthLimits, error) {
	res := garden.BandwidthLimits{}
	err := c.do(
		routes.LimitBandwidth,
		limits,
		&res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	return res, err
}

func (c *connection) CurrentBandwidthLimits(handle string) (garden.BandwidthLimits, error) {
	res := garden.BandwidthLimits{}

	err := c.do(
		routes.CurrentBandwidthLimits,
		nil,
		&res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	return res, err
}

func (c *connection) LimitCPU(handle string, limits garden.CPULimits) (garden.CPULimits, error) {
	res := garden.CPULimits{}

	err := c.do(
		routes.LimitCPU,
		limits,
		&res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	return res, err
}

func (c *connection) CurrentCPULimits(handle string) (garden.CPULimits, error) {
	res := garden.CPULimits{}

	err := c.do(
		routes.CurrentCPULimits,
		nil,
		&res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	return res, err
}

func (c *connection) LimitDisk(handle string, limits garden.DiskLimits) (garden.DiskLimits, error) {
	res := garden.DiskLimits{}

	err := c.do(
		routes.LimitDisk,
		limits,
		&res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	return res, err
}

func (c *connection) CurrentDiskLimits(handle string) (garden.DiskLimits, error) {
	res := garden.DiskLimits{}

	err := c.do(
		routes.CurrentDiskLimits,
		nil,
		&res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	return res, err
}

func (c *connection) LimitMemory(handle string, limits garden.MemoryLimits) (garden.MemoryLimits, error) {
	res := garden.MemoryLimits{}

	err := c.do(
		routes.LimitMemory,
		limits,
		&res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	return res, err
}

func (c *connection) CurrentMemoryLimits(handle string) (garden.MemoryLimits, error) {
	res := garden.MemoryLimits{}

	err := c.do(
		routes.CurrentMemoryLimits,
		nil,
		&res,
		rata.Params{
			"handle": handle,
		},
		nil,
	)

	return res, err
}

func (c *connection) StreamIn(handle string, dstPath string, reader io.Reader) error {
	body, err := c.doStream(
		routes.StreamIn,
		reader,
		rata.Params{
			"handle": handle,
		},
		url.Values{
			"destination": []string{dstPath},
		},
		"application/x-tar",
	)
	if err != nil {
		return err
	}

	return body.Close()
}

func (c *connection) StreamOut(handle string, srcPath string) (io.ReadCloser, error) {
	return c.doStream(
		routes.StreamOut,
		nil,
		rata.Params{
			"handle": handle,
		},
		url.Values{
			"source": []string{srcPath},
		},
		"",
	)
}

func (c *connection) List(filterProperties garden.Properties) ([]string, error) {
	values := url.Values{}
	for name, val := range filterProperties {
		values[name] = []string{val}
	}

	res := &struct {
		Handles []string
	}{}

	if err := c.do(
		routes.List,
		nil,
		&res,
		nil,
		values,
	); err != nil {
		return nil, err
	}

	return res.Handles, nil
}

func (c *connection) Properties(handle string) (garden.Properties, error) {
	res := make(garden.Properties)
	err := c.do(routes.Properties, nil, &res, rata.Params{"handle": handle}, nil)
	return res, err
}

func (c *connection) Metrics(handle string) (garden.Metrics, error) {
	res := garden.Metrics{}
	err := c.do(routes.Metrics, nil, &res, rata.Params{"handle": handle}, nil)
	return res, err
}

func (c *connection) Info(handle string) (garden.ContainerInfo, error) {
	res := garden.ContainerInfo{}

	err := c.do(routes.Info, nil, &res, rata.Params{"handle": handle}, nil)
	if err != nil {
		return garden.ContainerInfo{}, err
	}

	return res, nil
}

func (c *connection) BulkInfo(handles []string) (map[string]garden.ContainerInfoEntry, error) {
	res := make(map[string]garden.ContainerInfoEntry)
	queryParams := url.Values{
		"handles": []string{strings.Join(handles, ",")},
	}
	err := c.do(routes.BulkInfo, nil, &res, nil, queryParams)
	return res, err
}

func (c *connection) BulkMetrics(handles []string) (map[string]garden.ContainerMetricsEntry, error) {
	res := make(map[string]garden.ContainerMetricsEntry)
	queryParams := url.Values{
		"handles": []string{strings.Join(handles, ",")},
	}
	err := c.do(routes.BulkMetrics, nil, &res, nil, queryParams)
	return res, err
}

func (c *connection) do(
	handler string,
	req, res interface{},
	params rata.Params,
	query url.Values,
) error {
	var body io.Reader

	if req != nil {
		buf := new(bytes.Buffer)

		err := transport.WriteMessage(buf, req)
		if err != nil {
			return err
		}

		body = buf
	}

	contentType := ""
	if req != nil {
		contentType = "application/json"
	}

	response, err := c.doStream(
		handler,
		body,
		params,
		query,
		contentType,
	)
	if err != nil {
		return err
	}

	defer response.Close()

	return json.NewDecoder(response).Decode(res)
}

func (c *connection) doStream(
	handler string,
	body io.Reader,
	params rata.Params,
	query url.Values,
	contentType string,
) (io.ReadCloser, error) {
	request, err := c.req.CreateRequest(handler, params, body)
	if err != nil {
		return nil, err
	}

	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	if query != nil {
		request.URL.RawQuery = query.Encode()
	}

	httpResp, err := c.noKeepaliveClient.Do(request)
	if err != nil {
		return nil, err
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode > 299 {
		errResponse, err := ioutil.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("bad response: %s", httpResp.Status)
		}

		if httpResp.StatusCode == http.StatusServiceUnavailable {
			// The body has the actual error string formed at the server.
			return nil, garden.NewServiceUnavailableError(string(errResponse))
		}

		return nil, Error{httpResp.StatusCode, string(errResponse)}
	}

	return httpResp.Body, nil
}
