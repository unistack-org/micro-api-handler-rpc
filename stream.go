package rpc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gobwas/httphead"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/unistack-org/micro/v3/api"
	"github.com/unistack-org/micro/v3/client"
	"github.com/unistack-org/micro/v3/codec"
	"github.com/unistack-org/micro/v3/logger"
	"github.com/unistack-org/micro/v3/util/router"
)

// serveWebsocket will stream rpc back over websockets assuming json
func serveWebsocket(ctx context.Context, w http.ResponseWriter, r *http.Request, service *api.Service, c client.Client) {
	var op ws.OpCode

	ct := r.Header.Get("Content-Type")
	// Strip charset from Content-Type (like `application/json; charset=UTF-8`)
	if idx := strings.IndexRune(ct, ';'); idx >= 0 {
		ct = ct[:idx]
	}

	// create custom router
	callOpts := []client.CallOption{client.WithRouter(router.New(service.Services))}

	if t := r.Header.Get("Timeout"); t != "" {
		// assume timeout integer secodns
		if td, err := time.ParseDuration(t + "s"); err == nil {
			callOpts = append(callOpts, client.WithRequestTimeout(td))
		}
	}

	// check proto from request
	switch ct {
	case "application/json":
		op = ws.OpText
	default:
		op = ws.OpBinary
	}

	hdr := make(http.Header)
	if proto, ok := r.Header["Sec-WebSocket-Protocol"]; ok {
		for _, p := range proto {
			switch p {
			case "binary":
				hdr["Sec-WebSocket-Protocol"] = []string{"binary"}
				op = ws.OpBinary
			default:
				op = ws.OpBinary
			}
		}
	}
	payload, err := requestPayload(r)
	if err != nil {
		if logger.V(logger.ErrorLevel) {
			logger.Error(ctx, err.Error())
		}
		return
	}

	upgrader := ws.HTTPUpgrader{Timeout: 5 * time.Second,
		Protocol: func(proto string) bool {
			if strings.Contains(proto, "binary") {
				return true
			}
			// fallback to support all protocols now
			return true
		},
		Extension: func(httphead.Option) bool {
			// disable extensions for compatibility
			return false
		},
		Header: hdr,
	}

	conn, rw, _, err := upgrader.Upgrade(r, w)
	if err != nil {
		if logger.V(logger.ErrorLevel) {
			logger.Error(ctx, err.Error())
		}
		return
	}

	defer func() {
		if err := conn.Close(); err != nil {
			if logger.V(logger.ErrorLevel) {
				logger.Error(ctx, err.Error())
			}
			return
		}
	}()

	var request interface{}

	switch ct {
	case "application/json":
		m := json.RawMessage(payload)
		request = &m
	default:
		request = &codec.Frame{Data: payload}
	}

	// we always need to set content type for message
	if ct == "" {
		ct = "application/json"
	}

	req := c.NewRequest(
		service.Name,
		service.Endpoint.Name,
		request,
		client.RequestContentType(ct),
		client.StreamingRequest(true),
	)

	// create a new stream
	stream, err := c.Stream(ctx, req, callOpts...)
	if err != nil {
		if logger.V(logger.ErrorLevel) {
			logger.Error(ctx, err.Error())
		}
		return
	}

	if request != nil {
		if err = stream.Send(request); err != nil {
			if logger.V(logger.ErrorLevel) {
				logger.Error(ctx, err.Error())
			}
			return
		}
	}

	go writeLoop(rw, stream)

	rsp := stream.Response()

	// receive from stream and send to client
	for {
		select {
		case <-ctx.Done():
			return
		case <-stream.Context().Done():
			return
		default:
			// read backend response body
			buf, err := rsp.Read()
			if err != nil {
				// wants to avoid import  grpc/status.Status
				if strings.Contains(err.Error(), "context canceled") {
					return
				}
				if logger.V(logger.ErrorLevel) {
					logger.Error(ctx, err.Error())
				}
				return
			}

			// write the response
			if err = wsutil.WriteServerMessage(rw, op, buf); err != nil {
				if logger.V(logger.ErrorLevel) {
					logger.Error(ctx, err.Error())
				}
				return
			}
			if err = rw.Flush(); err != nil {
				if logger.V(logger.ErrorLevel) {
					logger.Error(ctx, err.Error())
				}
				return
			}
		}
	}
}

// writeLoop
func writeLoop(rw io.ReadWriter, stream client.Stream) {
	// close stream when done
	defer stream.Close()

	for {
		select {
		case <-stream.Context().Done():
			return
		default:
			buf, op, err := wsutil.ReadClientData(rw)
			if err != nil {
				if wserr, ok := err.(wsutil.ClosedError); ok {
					switch wserr.Code {
					case ws.StatusGoingAway:
						// this happens when user leave the page
						return
					case ws.StatusNormalClosure, ws.StatusNoStatusRcvd:
						// this happens when user close ws connection, or we don't get any status
						return
					}
				}
				if logger.V(logger.ErrorLevel) {
					logger.Error(stream.Context(), err.Error())
				}
				return
			}
			switch op {
			default:
				// not relevant
				continue
			case ws.OpText, ws.OpBinary:
				break
			}
			// send to backend
			// default to trying json
			// if the extracted payload isn't empty lets use it
			request := &codec.Frame{Data: buf}
			if err := stream.Send(request); err != nil {
				if logger.V(logger.ErrorLevel) {
					logger.Error(stream.Context(), err.Error())
				}
				return
			}
		}
	}
}

func isStream(r *http.Request, srv *api.Service) bool {
	// check if it's a web socket
	if !isWebSocket(r) {
		return false
	}
	// check if the endpoint supports streaming
	for _, service := range srv.Services {
		for _, ep := range service.Endpoints {
			// skip if it doesn't match the name
			if ep.Name != srv.Endpoint.Name {
				continue
			}
			// matched if the name
			if v := ep.Metadata["stream"]; v == "true" {
				return true
			}
		}
	}
	return false
}

func isWebSocket(r *http.Request) bool {
	contains := func(key, val string) bool {
		vv := strings.Split(r.Header.Get(key), ",")
		for _, v := range vv {
			if val == strings.ToLower(strings.TrimSpace(v)) {
				return true
			}
		}
		return false
	}

	if contains("Connection", "upgrade") && contains("Upgrade", "websocket") {
		return true
	}

	return false
}
