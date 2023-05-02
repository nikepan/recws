// Package recws provides websocket client based on gorilla/websocket
// that will automatically reconnect if the connection is dropped.
package recws

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jpillora/backoff"
)

// ErrNotConnected is returned when the application read/writes
// a message and the connection is closed
var ErrNotConnected = errors.New("websocket: not connected")

// The RecConn type represents a Reconnecting WebSocket connection.
type RecConn struct {
	// RecIntvlMin specifies the initial reconnecting interval,
	// default to 2 seconds
	RecIntvlMin time.Duration
	// RecIntvlMax specifies the maximum reconnecting interval,
	// default to 30 seconds
	RecIntvlMax time.Duration
	// RecIntvlFactor specifies the rate of increase of the reconnection
	// interval, default to 1.5
	RecIntvlFactor float64
	// HandshakeTimeout specifies the duration for the handshake to complete,
	// default to 2 seconds
	HandshakeTimeout time.Duration
	// Proxy specifies the proxy function for the dialer
	// defaults to ProxyFromEnvironment
	Proxy func(*http.Request) (*url.URL, error)
	// Client TLS config to use on reconnect
	TLSClientConfig *tls.Config
	// SubscribeHandler fires after the connection successfully establish. Must be quick
	SubscribeHandler func() error
	// KeepAliveTimeout is an interval for sending ping/pong messages
	// disabled if 0
	KeepAliveTimeout time.Duration
	// ReconnectHandler signals of reconnects for metrics. Must be quick
	ReconnectHandler func()
	// LogHandler handles all log messages
	LogHandler func(v LogValues)
	// NonVerbose suppress connecting/reconnecting messages.
	NonVerbose bool
	// AllowKeepAliveDataResponse allows recognize data response like keepalive response
	AllowKeepAliveDataResponse bool

	isConnected       bool
	mu                sync.RWMutex
	url               string
	reqHeader         http.Header
	httpResp          *http.Response
	dialErr           error
	dialer            *websocket.Dialer
	keepAliveResponse *keepAliveResponse
	manualReconnect   bool

	*websocket.Conn
}

// LogValues type includes values for send to logger
type LogValues struct {
	// Msg is main message
	Msg string
	// Err is error for separate and display it
	Err error
	// Url is connection url
	Url string
	// Fatal is tag of fatal error
	Fatal bool
}

func (rc *RecConn) handleReconnect() {
	rc.mu.RLock()
	handler := rc.ReconnectHandler
	rc.mu.RUnlock()
	if handler != nil {
		handler()
	}
}

// CloseAndReconnect will try to reconnect.
func (rc *RecConn) CloseAndReconnect() {
	if !rc.getManualReconnect() {
		rc.Close()
		rc.handleReconnect()
		go rc.connect()
	}
}

// ManualCloseAndReconnect will try to reconnect.
func (rc *RecConn) ManualCloseAndReconnect() {
	rc.setManualReconnect(true)
	rc.Close()
	rc.handleReconnect()
	go rc.connect()
}

// setIsConnected sets state for isConnected
func (rc *RecConn) setIsConnected(state bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.isConnected = state
}

func (rc *RecConn) getConn() *websocket.Conn {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.Conn
}

// Close closes the underlying network connection without
// sending or waiting for a close frame.
func (rc *RecConn) Close() {
	rc.mu.Lock()
	if rc.Conn != nil {
		rc.Conn.Close()
	}
	rc.isConnected = false
	rc.mu.Unlock()
}

// Shutdown gracefully closes the connection by sending the websocket.CloseMessage.
// The writeWait param defines the duration before the deadline of the write operation is hit.
func (rc *RecConn) Shutdown(writeWait time.Duration) {
	msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
	err := rc.WriteControl(websocket.CloseMessage, msg, time.Now().Add(writeWait))
	if err != nil && err != websocket.ErrCloseSent {
		// If close message could not be sent, then close without the handshake.
		rc.log(LogValues{Err: err, Msg: "Shutdown"})
		rc.Close()
	}
}

func (rc *RecConn) getManualReconnect() bool {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.manualReconnect
}

func (rc *RecConn) setManualReconnect(v bool) {
	rc.mu.Lock()
	rc.manualReconnect = v
	defer rc.mu.Unlock()
}

// ReadMessage is a helper method for getting a reader
// using NextReader and reading from that reader to a buffer.
//
// If the connection is closed ErrNotConnected is returned
func (rc *RecConn) ReadMessage() (messageType int, message []byte, err error) {
	err = ErrNotConnected
	if rc.IsConnected() {
		conn := rc.getConn()
		messageType, message, err = conn.ReadMessage()
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			rc.Close()
			return messageType, message, nil
		}
		if err != nil && conn == rc.getConn() {
			rc.CloseAndReconnect()
		}
		if err == nil {
			rc.getKeepAliveResponse().setLastDataResponse()
		}
	}

	return
}

// WriteMessage is a helper method for getting a writer using NextWriter,
// writing the message and closing the writer.
//
// If the connection is closed ErrNotConnected is returned
func (rc *RecConn) WriteMessage(messageType int, data []byte) error {
	err := ErrNotConnected
	if rc.IsConnected() {
		rc.mu.Lock()
		err = rc.Conn.WriteMessage(messageType, data)
		rc.mu.Unlock()
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			rc.Close()
			return nil
		}
		if err != nil {
			rc.CloseAndReconnect()
		}
	}

	return err
}

// WriteJSON writes the JSON encoding of v to the connection.
//
// See the documentation for encoding/json Marshal for details about the
// conversion of Go values to JSON.
//
// If the connection is closed ErrNotConnected is returned
func (rc *RecConn) WriteJSON(v interface{}) error {
	err := ErrNotConnected
	if rc.IsConnected() {
		rc.mu.Lock()
		err = rc.Conn.WriteJSON(v)
		rc.mu.Unlock()
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			rc.Close()
			return nil
		}
		if err != nil {
			rc.CloseAndReconnect()
		}
	}

	return err
}

// ReadJSON reads the next JSON-encoded message from the connection and stores
// it in the value pointed to by v.
//
// See the documentation for the encoding/json Unmarshal function for details
// about the conversion of JSON to a Go value.
//
// If the connection is closed ErrNotConnected is returned
func (rc *RecConn) ReadJSON(v interface{}) error {
	err := ErrNotConnected
	if rc.IsConnected() {
		conn := rc.getConn()
		err = conn.ReadJSON(v)
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			rc.Close()
			return nil
		}
		if err != nil && conn == rc.getConn() {
			rc.CloseAndReconnect()
		}
		if err == nil {
			rc.getKeepAliveResponse().setLastDataResponse()
		}
	}

	return err
}

func (rc *RecConn) getKeepAliveResponse() *keepAliveResponse {
	rc.mu.RLock()
	ka := rc.keepAliveResponse
	rc.mu.RUnlock()
	return ka
}

func (rc *RecConn) setURL(url string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.url = url
}

func (rc *RecConn) setReqHeader(reqHeader http.Header) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.reqHeader = reqHeader
}

// parseURL parses current url
func (rc *RecConn) parseURL(urlStr string) (string, error) {
	if urlStr == "" {
		return "", errors.New("dial: url cannot be empty")
	}

	u, err := url.Parse(urlStr)

	if err != nil {
		return "", errors.New("url: " + err.Error())
	}

	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", errors.New("url: websocket uris must start with ws or wss scheme")
	}

	if u.User != nil {
		return "", errors.New("url: user name and password are not allowed in websocket URIs")
	}

	return urlStr, nil
}

func (rc *RecConn) setDefaultRecIntvlMin() {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.RecIntvlMin == 0 {
		rc.RecIntvlMin = 2 * time.Second
	}
}

func (rc *RecConn) setDefaultRecIntvlMax() {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.RecIntvlMax == 0 {
		rc.RecIntvlMax = 30 * time.Second
	}
}

func (rc *RecConn) setDefaultRecIntvlFactor() {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.RecIntvlFactor == 0 {
		rc.RecIntvlFactor = 1.5
	}
}

func (rc *RecConn) setDefaultHandshakeTimeout() {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.HandshakeTimeout == 0 {
		rc.HandshakeTimeout = 2 * time.Second
	}
}

func (rc *RecConn) setDefaultProxy() {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.Proxy == nil {
		rc.Proxy = http.ProxyFromEnvironment
	}
}

func (rc *RecConn) setDefaultDialer(tlsClientConfig *tls.Config, handshakeTimeout time.Duration) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.dialer = &websocket.Dialer{
		HandshakeTimeout: handshakeTimeout,
		Proxy:            rc.Proxy,
		TLSClientConfig:  tlsClientConfig,
	}
}

func (rc *RecConn) getHandshakeTimeout() time.Duration {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.HandshakeTimeout
}

func (rc *RecConn) getTLSClientConfig() *tls.Config {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.TLSClientConfig
}

func (rc *RecConn) SetTLSClientConfig(tlsClientConfig *tls.Config) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	rc.TLSClientConfig = tlsClientConfig
}

// Dial creates a new client connection.
// The URL url specifies the host and request URI. Use requestHeader to specify
// the origin (Origin), subprotocols (Sec-WebSocket-Protocol) and cookies
// (Cookie). Use GetHTTPResponse() method for the response.Header to get
// the selected subprotocol (Sec-WebSocket-Protocol) and cookies (Set-Cookie).
func (rc *RecConn) Dial(urlStr string, reqHeader http.Header) error {
	urlStr, err := rc.parseURL(urlStr)

	if err != nil {
		rc.log(LogValues{Msg: "Dial", Err: err, Fatal: true})
		return err
	}

	// Config
	rc.setURL(urlStr)
	rc.setReqHeader(reqHeader)
	rc.setDefaultRecIntvlMin()
	rc.setDefaultRecIntvlMax()
	rc.setDefaultRecIntvlFactor()
	rc.setDefaultHandshakeTimeout()
	rc.setDefaultProxy()
	rc.setDefaultDialer(rc.getTLSClientConfig(), rc.getHandshakeTimeout())

	// Connect
	go rc.connect()

	// wait on first attempt
	time.Sleep(rc.getHandshakeTimeout())
	return nil
}

// GetURL returns current connection url
func (rc *RecConn) GetURL() string {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.url
}

func (rc *RecConn) log(v LogValues) {
	if rc.LogHandler != nil {
		rc.LogHandler(v)
	} else if v.Fatal {
		log.Fatalf("FATAL: %+v: %+v (%+v)\n", v.Msg, v.Err, v.Url)
	} else if v.Err != nil {
		log.Printf("ERROR: %+v: %+v (%+v)\n", v.Msg, v.Err, v.Url)
	} else {
		log.Printf("%+v (%+v)\n", v.Msg, v.Url)
	}
}

func (rc *RecConn) getNonVerbose() bool {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.NonVerbose
}

func (rc *RecConn) getBackoff() *backoff.Backoff {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return &backoff.Backoff{
		Min:    rc.RecIntvlMin,
		Max:    rc.RecIntvlMax,
		Factor: rc.RecIntvlFactor,
		Jitter: true,
	}
}

func (rc *RecConn) hasSubscribeHandler() bool {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.SubscribeHandler != nil
}

func (rc *RecConn) getKeepAliveTimeout() time.Duration {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.KeepAliveTimeout
}

func (rc *RecConn) writeControlPingMessage() error {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	return rc.Conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second))
}

func (rc *RecConn) keepAlive() {
	var (
		ticker = time.NewTicker(rc.getKeepAliveTimeout())
	)

	rc.mu.Lock()
	rc.Conn.SetPongHandler(func(msg string) error {
		rc.getKeepAliveResponse().setLastResponse()
		return nil
	})
	rc.mu.Unlock()

	go func() {
		defer ticker.Stop()

		for {
			if !rc.IsConnected() {
				continue
			}

			if err := rc.writeControlPingMessage(); err != nil {
				rc.log(LogValues{Err: err})
			}

			<-ticker.C
			timeoutOffset := time.Millisecond * 500
			if time.Since(rc.getKeepAliveResponse().getLastResponse()) > rc.getKeepAliveTimeout()+timeoutOffset {
				rc.log(LogValues{Err: errors.New("keepalive timeout"), Msg: "Reconnect", Url: rc.url})
				rc.ManualCloseAndReconnect()
				return
			}
		}
	}()
}

func (rc *RecConn) connect() {
	b := rc.getBackoff()
	rand.Seed(time.Now().UTC().UnixNano())

	for {
		nextItvl := b.Duration()
		if !rc.getNonVerbose() {
			rc.log(LogValues{Msg: "Dial: start", Url: rc.url})
		}
		wsConn, httpResp, err := rc.dialer.Dial(rc.url, rc.reqHeader)

		rc.setManualReconnect(false)

		rc.mu.Lock()
		rc.Conn = wsConn
		rc.dialErr = err
		rc.isConnected = err == nil
		rc.httpResp = httpResp
		if rc.keepAliveResponse == nil {
			rc.keepAliveResponse = new(keepAliveResponse)
			rc.keepAliveResponse.allowDataResponse = rc.AllowKeepAliveDataResponse
		}
		rc.mu.Unlock()

		if err == nil {
			if !rc.getNonVerbose() {
				rc.log(LogValues{Msg: "Dial: connection was successfully established", Url: rc.url})
			}

			if rc.hasSubscribeHandler() {
				if err := rc.SubscribeHandler(); err != nil {
					rc.log(LogValues{Msg: "Dial: connect handler failed", Err: err, Fatal: true})
				} else if !rc.getNonVerbose() {
					rc.log(LogValues{Msg: "Dial: connect handler was successfully established", Url: rc.url})
				}
			}

			if rc.getKeepAliveTimeout() != 0 {
				rc.keepAlive()
			}

			return
		}

		if !rc.getNonVerbose() {
			rc.log(LogValues{Err: err, Msg: fmt.Sprintf("Dial: will try again in %+v seconds", nextItvl), Url: rc.url})
		}

		time.Sleep(nextItvl)
	}
}

// GetHTTPResponse returns the http response from the handshake.
// Useful when WebSocket handshake fails,
// so that callers can handle redirects, authentication, etc.
func (rc *RecConn) GetHTTPResponse() *http.Response {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.httpResp
}

// GetDialError returns the last dialer error.
// nil on successful connection.
func (rc *RecConn) GetDialError() error {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.dialErr
}

// IsConnected returns the WebSocket connection state
func (rc *RecConn) IsConnected() bool {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.isConnected
}
