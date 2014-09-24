/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package simplepush

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"code.google.com/p/go.net/websocket"

	"github.com/mozilla-services/pushgo/id"
)

//    -- Workers
//      these write back to the websocket.

type Worker struct {
	app          *Application
	logger       *SimpleLogger
	id           string
	state        WorkerState
	stopped      bool
	maxChannels  int
	lastPing     time.Time
	pingInt      time.Duration
	metrics      *Metrics
	helloTimeout time.Duration
}

type WorkerState int

const (
	WorkerInactive WorkerState = 0
	WorkerActive               = 1
)

type RequestHeader struct {
	Type string `json:"messageType"`
}

type HelloRequest struct {
	DeviceID   *string         `json:"uaid"`
	ChannelIDs []interface{}   `json:"channelIDs"`
	PingData   json.RawMessage `json:"connect"`
}

type RegisterRequest struct {
	ChannelID string `json:"channelID"`
}

type RegisterReply struct {
	Type      string `json:"messageType"`
	DeviceID  string `json:"uaid"`
	Status    int    `json:"status"`
	ChannelID string `json:"channelID"`
	Endpoint  string `json:"pushEndpoint"`
}

type UnregisterRequest struct {
	ChannelID string `json:"channelID"`
}

type UnregisterReply struct {
	Type      string `json:"messageType"`
	Status    int    `json:"status"`
	ChannelID string `json:"channelID"`
}

type FlushReply struct {
	Type    string   `json:"messageType"`
	Updates []Update `json:"updates,omitempty"`
	Expired []string `json:"expired,omitempty"`
}

type ACKRequest struct {
	Updates []Update `json:"update"`
	Expired []string `json:"expired"`
}

type PingReply struct {
	Type   string `json:"messageType"`
	Status int    `json:"status"`
}

const CHID_DEFAULT_MAX_NUM = 200

func NewWorker(app *Application, id string) *Worker {
	return &Worker{
		app:          app,
		logger:       app.Logger(),
		metrics:      app.Metrics(),
		id:           id,
		state:        WorkerActive,
		stopped:      false,
		pingInt:      app.clientMinPing,
		maxChannels:  app.Store().MaxChannels(),
		helloTimeout: app.clientHelloTimeout,
	}
}

func (self *Worker) sniffer(sock *PushWS) {
	// Sniff the websocket for incoming data.
	// Reading from the websocket is a blocking operation, and we also
	// need to write out when an even occurs. This isolates the incoming
	// reads to a separate go process.
	var (
		socket = sock.Socket
		raw    []byte
		//eofCount    int    = 0
		err error
	)

	for {
		// declare buffer here so that the struct is cleared between msgs.
		raw = raw[:0]
		err = nil

		// Were we told to shut down?
		if self.stopped {
			return
		}
		if err = websocket.Message.Receive(socket, &raw); err != nil {
			self.stopped = true
			if self.logger.ShouldLog(ERROR) {
				self.logger.Error("worker", "Websocket Error",
					LogFields{"rid": self.id, "error": ErrStr(err)})
			}
			continue
		}
		if len(raw) <= 0 {
			continue
		}

		//eofCount = 0
		//ignore {} pings for logging purposes.
		if len(raw) > 5 {
			if self.logger.ShouldLog(INFO) {
				self.logger.Info("worker", "Socket receive",
					LogFields{"rid": self.id, "raw": string(raw)})
			}
		}
		isPing, err := isPingBody(raw)
		if err != nil {
			if self.logger.ShouldLog(WARNING) {
				self.logger.Warn("worker", "Malformed request payload",
					LogFields{"rid": self.id, "raw": string(raw), "error": ErrStr(err)})
			}
			self.stopped = true
			continue
		}
		header := new(RequestHeader)
		if isPing {
			header.Type = "ping"
		} else if err = json.Unmarshal(raw, header); err != nil {
			if typeErr, ok := err.(*json.UnmarshalTypeError); ok {
				if self.logger.ShouldLog(WARNING) {
					self.logger.Warn("worker", "Mismatched header field types", LogFields{
						"rid":      self.id,
						"expected": typeErr.Type.String(),
						"actual":   typeErr.Value})
				}
				self.handleError(sock, raw, ErrUnknownCommand)
			} else if syntaxErr, ok := err.(*json.SyntaxError); ok {
				if self.logger.ShouldLog(WARNING) {
					self.logger.Warn("worker", "Malformed request payload", LogFields{
						"rid":      self.id,
						"expected": string(raw[:syntaxErr.Offset]),
						"error":    syntaxErr.Error()})
				}
			} else {
				if self.logger.ShouldLog(WARNING) {
					self.logger.Warn("worker", "Error parsing request payload",
						LogFields{"rid": self.id, "error": ErrStr(err)})
				}
			}
			self.stopped = true
			continue
		}
		switch strings.ToLower(header.Type) {
		case "ping":
			err = self.Ping(sock, header, raw)
		case "hello":
			err = self.Hello(sock, header, raw)
		case "ack":
			err = self.Ack(sock, header, raw)
		case "register":
			err = self.Register(sock, header, raw)
		case "unregister":
			err = self.Unregister(sock, header, raw)
		case "purge":
			err = self.Purge(sock, header, raw)
		default:
			if self.logger.ShouldLog(WARNING) {
				self.logger.Warn("worker", "Bad command",
					LogFields{"rid": self.id, "cmd": header.Type})
			}
			err = ErrUnknownCommand
		}
		if err != nil {
			if self.logger.ShouldLog(DEBUG) {
				self.logger.Debug("worker", "Run returned error",
					LogFields{"rid": self.id, "cmd": header.Type, "error": ErrStr(err)})
			}
			self.handleError(sock, raw, err)
			self.stopped = true
			continue
		}
	}
}

// standardize the error reporting back to the client.
func (self *Worker) handleError(sock *PushWS, message []byte, err error) (ret error) {
	if self.logger.ShouldLog(INFO) {
		self.logger.Info("worker", "Sending error",
			LogFields{"rid": self.id, "error": ErrStr(err)})
	}
	reply := make(map[string]interface{})
	if ret = json.Unmarshal(message, &reply); ret != nil {
		return
	}
	reply["status"], reply["error"] = ErrToStatus(err)
	return websocket.JSON.Send(sock.Socket, reply)
}

// General workhorse loop for the websocket handler.
func (self *Worker) Run(sock *PushWS) {
	time.AfterFunc(self.helloTimeout,
		func() {
			if sock.Uaid == "" {
				if self.logger.ShouldLog(DEBUG) {
					self.logger.Debug("dash", "Worker Idle connection. Closing socket",
						LogFields{"rid": self.id})
				}
				sock.Socket.Close()
			}
		})

	defer func(sock *PushWS) {
		if r := recover(); r != nil {
			if err, _ := r.(error); err != nil && self.logger.ShouldLog(ERROR) {
				self.logger.Error("worker", "Unhandled connection error",
					LogFields{"rid": self.id, "error": ErrStr(err)})
			}
			sock.Socket.Close()
		}
		return
	}(sock)

	self.sniffer(sock)
	sock.Socket.Close()

	if self.logger.ShouldLog(INFO) {
		self.logger.Info("dash", "Run has completed a shut-down",
			LogFields{"rid": self.id})
	}
}

// Associate the UAID for this socket connection (and flush any data that
// may be pending for the connection)
func (self *Worker) Hello(sock *PushWS, header *RequestHeader, message []byte) (err error) {
	// register the UAID
	defer func() {
		if r := recover(); r != nil {
			debug.PrintStack()
			if err, _ := r.(error); err != nil && self.logger.ShouldLog(ERROR) {
				self.logger.Error("worker", "Unhandled error",
					LogFields{"rid": self.id, "cmd": "hello", "error": ErrStr(err)})
			}
			err = ErrInvalidParams
		}
	}()

	//Force the client to re-register all it's clients.
	// This is done by returning a new UAID.
	var (
		forceReset    bool
		suggestedUAID string
	)

	request := new(HelloRequest)
	if err = json.Unmarshal(message, request); err != nil {
		return ErrInvalidParams
	}
	if request.DeviceID == nil {
		return ErrInvalidParams
	}
	suggestedUAID = *request.DeviceID
	/* NOTE: This seems to be a redirect, which I don't believe we support
	if redir := self.config.Get("db.redirect", ""); len(redir) > 0 {
		statusCode := 302
		resp := JsMap{
			"messageType": header.Type,
			"status":      statusCode,
			"redirect":    redir,
			"uaid":        sock.Uaid}
		if self.logger.ShouldLog(DEBUG) {
			self.logger.Debug("worker", "sending redirect", LogFields{
				"rid":      self.id,
				"cmd":      header.Type,
				"code":     strconv.FormatInt(int64(statusCode), 10),
				"redirect": redir,
				"uaid":     suggestedUAID})
		}
		websocket.JSON.Send(sock.Socket, resp)
		return nil
	} */
	if request.ChannelIDs == nil {
		// Must include "channelIDs" (even if empty)
		if self.logger.ShouldLog(DEBUG) {
			self.logger.Debug("worker", "Missing ChannelIDs",
				LogFields{"rid": self.id})
		}
		return ErrNoParams
	}
	if len(sock.Uaid) > 0 {
		if len(suggestedUAID) == 0 || sock.Uaid == suggestedUAID {
			// Duplicate handshake with omitted or identical device ID.
			goto registerDevice
		}
		// if there's already a Uaid for this device, don't accept a new one
		if self.logger.ShouldLog(DEBUG) {
			self.logger.Debug("worker", "Conflicting UAIDs",
				LogFields{"rid": self.id})
		}
		return ErrExistingID
	}
	if forceReset = len(suggestedUAID) == 0; forceReset {
		if self.logger.ShouldLog(DEBUG) {
			self.logger.Debug("worker", "Generating new UAID for device",
				LogFields{"rid": self.id})
		}
		goto registerDevice
	}
	if !id.Valid(suggestedUAID) {
		if self.logger.ShouldLog(DEBUG) {
			self.logger.Debug("worker", "Invalid character in UAID",
				LogFields{"rid": self.id})
		}
		return ErrInvalidID
	}
	// if there's no UAID for the socket, accept or create a new one.
	if forceReset = self.app.ClientExists(suggestedUAID); forceReset {
		if self.logger.ShouldLog(WARNING) {
			self.logger.Warn("worker", "UAID collision; resetting UAID for device",
				LogFields{"rid": self.id, "uaid": suggestedUAID})
		}
		goto registerDevice
	}
	// are there a suspicious number of channels?
	if forceReset = len(request.ChannelIDs) > self.maxChannels; forceReset {
		if self.logger.ShouldLog(WARNING) {
			self.logger.Warn("worker", "Too many channel IDs in handshake; resetting UAID", LogFields{
				"rid":         self.id,
				"uaid":        suggestedUAID,
				"channels":    strconv.Itoa(len(request.ChannelIDs)),
				"maxChannels": strconv.Itoa(self.maxChannels)})
		}
		sock.Store.DropAll(suggestedUAID)
		goto registerDevice
	}
	if forceReset = !sock.Store.Exists(sock.Uaid) && len(request.ChannelIDs) > 0; forceReset {
		if self.logger.ShouldLog(WARNING) {
			self.logger.Warn("worker", "Channel IDs specified in handshake for nonexistent UAID",
				LogFields{"rid": self.id, "uaid": suggestedUAID})
		}
		goto registerDevice
	}
	sock.Uaid = suggestedUAID

registerDevice:
	if forceReset {
		sock.Uaid, _ = id.Generate()
	}
	// register any proprietary connection requirements
	// alert the master of the new UAID.
	// It's not a bad idea from a security POV to only send
	// known args through to the server.
	cmd := PushCommand{
		Command: HELLO,
		Arguments: JsMap{
			"worker":  self,
			"uaid":    sock.Uaid,
			"chids":   request.ChannelIDs,
			"connect": []byte(request.PingData),
		},
	}
	// blocking call back to the boss.
	status, _ := self.app.Server().HandleCommand(cmd, sock)

	if self.logger.ShouldLog(DEBUG) {
		self.logger.Debug("worker", "sending response",
			LogFields{"rid": self.id, "cmd": "hello", "uaid": sock.Uaid})
	}
	// websocket.JSON.Send(sock.Socket, JsMap{
	// 	"messageType": header.Type,
	// 	"status":      status,
	// 	"uaid":        sock.Uaid})
	msg := []byte(fmt.Sprintf(`{"messageType":"%s","status":%d,"uaid":"%s"}`,
		header.Type, status, sock.Uaid))
	_, err = sock.Socket.Write(msg)
	self.metrics.Increment("updates.client.hello")
	if self.logger.ShouldLog(INFO) {
		self.logger.Info("dash", "Client successfully connected",
			LogFields{"rid": self.id})
	}
	self.state = WorkerActive
	if err == nil {
		// Get the lastAccessed time from wherever
		return self.Flush(sock, 0, "", 0)
	}
	return err
}

// Clear the data that the client stated it received, then re-flush any
// records (including new data)
func (self *Worker) Ack(sock *PushWS, header *RequestHeader, message []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if err, _ := r.(error); err != nil && self.logger.ShouldLog(ERROR) {
				self.logger.Error("worker", "Unhandled error",
					LogFields{"rid": self.id, "cmd": "ack", "error": ErrStr(err)})
			}
			debug.PrintStack()
			err = ErrInvalidParams
		}
	}()
	if sock.Uaid == "" {
		return ErrInvalidCommand
	}
	request := new(ACKRequest)
	if err = json.Unmarshal(message, request); err != nil {
		return ErrInvalidParams
	}
	if len(request.Updates) == 0 {
		return ErrNoParams
	}
	self.metrics.Increment("updates.client.ack")
	for _, update := range request.Updates {
		if err = sock.Store.Drop(sock.Uaid, update.ChannelID); err != nil {
			goto logError
		}
	}
	for _, channelID := range request.Expired {
		if err = sock.Store.Drop(sock.Uaid, channelID); err != nil {
			goto logError
		}
	}
	if self.logger.ShouldLog(DEBUG) {
		self.logger.Debug("worker", "sending response",
			LogFields{"rid": self.id, "cmd": "ack"})
	}
	// Get the lastAccessed time from wherever.
	return self.Flush(sock, 0, "", 0)
logError:
	if self.logger.ShouldLog(WARNING) {
		self.logger.Warn("worker", "sending response",
			LogFields{"rid": self.id, "cmd": "ack", "error": ErrStr(err)})
	}
	return err
}

// Register a new ChannelID. Optionally, encrypt the endpoint.
func (self *Worker) Register(sock *PushWS, header *RequestHeader, message []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if err, _ := r.(error); err != nil && self.logger.ShouldLog(ERROR) {
				self.logger.Error("worker", "Unhandled error",
					LogFields{"rid": self.id, "cmd": "register", "error": ErrStr(err)})
			}
			debug.PrintStack()
			err = ErrInvalidParams
		}
	}()

	if sock.Uaid == "" {
		return ErrInvalidCommand
	}
	request := new(RegisterRequest)
	if err = json.Unmarshal(message, request); err != nil || !id.Valid(request.ChannelID) {
		return ErrInvalidParams
	}
	if err = sock.Store.Register(sock.Uaid, request.ChannelID, 0); err != nil {
		if self.logger.ShouldLog(WARNING) {
			self.logger.Warn("worker", "Register failed, error updating backing store",
				LogFields{"rid": self.id, "cmd": "register", "error": ErrStr(err)})
		}
		return err
	}
	// have the server generate the callback URL.
	cmd := PushCommand{
		Command:   REGIS,
		Arguments: JsMap{"channelID": request.ChannelID},
	}
	status, args := self.app.Server().HandleCommand(cmd, sock)
	if self.logger.ShouldLog(DEBUG) {
		self.logger.Debug("worker", "Server returned", LogFields{
			"rid":  self.id,
			"cmd":  "register",
			"code": strconv.FormatInt(int64(status), 10),
			"chid": IStr(args["channelID"]),
			"uaid": IStr(args["uaid"])})
	}
	endpoint, _ := args["push.endpoint"].(string)
	// return the info back to the socket
	statusCode := 200
	if self.logger.ShouldLog(DEBUG) {
		self.logger.Debug("worker", "sending response", LogFields{
			"rid":          self.id,
			"cmd":          "register",
			"uaid":         sock.Uaid,
			"code":         strconv.FormatInt(int64(statusCode), 10),
			"channelID":    request.ChannelID,
			"pushEndpoint": endpoint})
	}
	websocket.JSON.Send(sock.Socket, RegisterReply{header.Type, sock.Uaid, statusCode, request.ChannelID, endpoint})
	self.metrics.Increment("updates.client.register")
	return err
}

// Unregister a ChannelID.
func (self *Worker) Unregister(sock *PushWS, header *RequestHeader, message []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if err, _ := r.(error); err != nil && self.logger.ShouldLog(ERROR) {
				self.logger.Error("worker", "Unhandled error",
					LogFields{"rid": self.id, "cmd": "register", "error": ErrStr(err)})
			}
			err = ErrInvalidParams
		}
	}()
	if sock.Uaid == "" {
		if self.logger.ShouldLog(WARNING) {
			self.logger.Warn("worker", "Unregister failed, missing sock.uaid",
				LogFields{"rid": self.id})
		}
		return ErrInvalidCommand
	}
	request := new(UnregisterRequest)
	if err = json.Unmarshal(message, request); err != nil {
		return ErrInvalidParams
	}
	if len(request.ChannelID) == 0 {
		if self.logger.ShouldLog(WARNING) {
			self.logger.Warn("worker", "Unregister failed, missing channelID",
				LogFields{"rid": self.id})
		}
		return ErrNoParams
	}
	// Always return success for an UNREG.
	if err = sock.Store.Unregister(sock.Uaid, request.ChannelID); err != nil {
		if self.logger.ShouldLog(WARNING) {
			self.logger.Warn("worker", "Unregister failed, error updating backing store",
				LogFields{"rid": self.id, "error": ErrStr(err)})
		}
	} else if self.logger.ShouldLog(DEBUG) {
		self.logger.Debug("worker", "sending response",
			LogFields{"rid": self.id, "cmd": "unregister"})
	}
	websocket.JSON.Send(sock.Socket, UnregisterReply{header.Type, 200, request.ChannelID})
	self.metrics.Increment("updates.client.unregister")
	return nil
}

// Dump any records associated with the UAID.
func (self *Worker) Flush(sock *PushWS, lastAccessed int64, channel string, version int64) (err error) {
	// flush pending data back to Client
	timer := time.Now()
	messageType := "notification"
	defer func(timer time.Time, sock *PushWS) {
		now := time.Now()
		if sock.Logger.ShouldLog(INFO) {
			sock.Logger.Info("timer",
				"Client flush completed",
				LogFields{"duration": strconv.FormatInt(int64(now.Sub(timer)), 10),
					"uaid": sock.Uaid})
		}
		self.metrics.Timer("client.flush", now.Sub(timer))
	}(timer, sock)
	if sock.Uaid == "" {
		if self.logger.ShouldLog(WARNING) {
			self.logger.Warn("worker", "Undefined UAID for socket. Aborting.",
				LogFields{"rid": self.id})
		}
		// Have the server clean up records associated with this UAID.
		// (Probably "none", but still good for housekeeping)
		self.stopped = true
		return nil
	}
	// Fetch the pending updates from #storage
	var (
		updates []Update
		reply   *FlushReply
	)
	mod := false
	// if we have a channel, don't flush. we can get them later in the ACK
	if len(channel) == 0 {
		var expired []string
		if updates, expired, err = sock.Store.FetchAll(sock.Uaid, time.Unix(lastAccessed, 0)); err != nil {
			if self.logger.ShouldLog(WARNING) {
				self.logger.Warn("worker", "Failed to flush Update to client.",
					LogFields{"rid": self.id, "uaid": sock.Uaid, "error": err.Error()})
			}
			return err
		}
		if len(updates) > 0 || len(expired) > 0 {
			reply = &FlushReply{messageType, updates, expired}
		}
	} else {
		// hand craft a notification update to the client.
		// TODO: allow bulk updates.
		updates = []Update{Update{channel, uint64(version)}}
		reply = &FlushReply{messageType, updates, nil}
	}
	if reply == nil {
		return nil
	}
	var logStrings []string
	if len(channel) > 0 {
		logStrings := make([]string, len(updates))
		prefix := ">>"
		if !mod {
			prefix = "+>"
		}
		for index, update := range updates {
			logStrings[index] = fmt.Sprintf("%s %s.%s = %d", prefix, sock.Uaid, update.ChannelID, update.Version)
			self.metrics.Increment("updates.sent")
		}
	}

	if self.logger.ShouldLog(DEBUG) {
		self.logger.Debug("worker", "Flushing data back to socket", LogFields{
			"rid":     self.id,
			"updates": fmt.Sprintf("[%s]", strings.Join(logStrings, ", "))})
	}
	websocket.JSON.Send(sock.Socket, reply)
	return nil
}

func (self *Worker) Ping(sock *PushWS, header *RequestHeader, _ []byte) (err error) {
	now := time.Now()
	if self.pingInt > 0 && !self.lastPing.IsZero() && now.Sub(self.lastPing) < self.pingInt {
		if self.logger.ShouldLog(WARNING) {
			self.logger.Warn("dash", "Client sending too many pings",
				LogFields{"rid": self.id, "source": sock.Socket.Config().Origin.String()})
		}
		self.stopped = true
		self.metrics.Increment("updates.client.too_many_pings")
		return ErrTooManyPings
	}
	self.lastPing = now
	if self.app.pushLongPongs {
		websocket.JSON.Send(sock.Socket, PingReply{header.Type, 200})
	} else {
		websocket.Message.Send(sock.Socket, []byte("{}"))
	}
	self.metrics.Increment("updates.client.ping")
	return nil
}

// TESTING func, purge associated records for this UAID
func (self *Worker) Purge(sock *PushWS, _ *RequestHeader, _ []byte) (err error) {
	/*
	   // If needed...
	   sock.Scmd <- PushCommand{Command: PURGE,
	       Arguments:JsMap{"uaid": sock.Uaid}}
	   result := <-sock.Scmd
	*/
	websocket.Message.Send(sock.Socket, []byte("{}"))
	return nil
}

func isPingBody(raw []byte) (bool, error) {
	if len(raw) < 2 || len(raw) == 2 && raw[0] == '{' && raw[1] == '}' {
		// Fast case: empty object literal; no whitespace.
		return true, nil
	}
	// Slower case: determine if the slice contains an empty object literal,
	// ignoring leading and trailing whitespace.
	var leftBraces, rightBraces int
	for _, b := range raw {
		switch b {
		case '{':
			leftBraces++
		case '}':
			rightBraces++
		case '\t', '\r', '\n', ' ':
			continue
		default:
			return false, nil
		}
	}
	if leftBraces <= 1 && leftBraces == rightBraces {
		return true, nil
	}
	// Quick sanity check for unbalanced or multiple consecutive braces.
	return false, ErrBadPayload
}

// o4fs
// vim: set tabstab=4 softtabstop=4 shiftwidth=4 noexpandtab
