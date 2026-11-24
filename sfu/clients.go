package sfu

import (
	"fmt"
	"strings"
	"time"

	"atomirex.com/umbrella/razor"
	"github.com/gorilla/websocket"
)

type RemoteClient interface {
	Label() string
	getStatus() *SFUStatusClient
	getStatusWithTimeout(timeout time.Duration) *SFUStatusClient
	stop()
	AddOutgoingTracksForIncomingTrack(*incomingTrack)
	RemoveOutgoingTracksForIncomingTrack(*incomingTrack)
	RequestEvalState()
}

type BaseClient struct {
	label  string
	logger *razor.Logger
}

func (bc *BaseClient) Label() string {
	return bc.label
}

type RemoteClientParameters struct {
	logger   *razor.Logger
	trunkurl string
	s        *Sfu
	ws       *websocket.Conn

	// observer receives connection lifecycle reports when this client represents an
	// outbound relay server connection. Nil for inbound web clients.
	observer RemoteClientObserver
}

type RemoteClientFactory interface {
	NewClient(params *RemoteClientParameters) RemoteClient
}

type DefaultRemoteClientFactory struct{}

// This can actually block ever returning . . . thanks to looping on the websocket
// not great
func (rcf *DefaultRemoteClientFactory) NewClient(params *RemoteClientParameters) RemoteClient {
	if params.trunkurl == "" {
		c := &client{
			BaseClient: BaseClient{
				label:  fmt.Sprintf("Incoming client from %s", params.ws.UnderlyingConn().RemoteAddr()),
				logger: params.logger,
			},

			websocket: params.ws,
		}

		c.run(params.ws, params.s)

		c.continueWebsocket(params.s)

		return c
	} else {
		if strings.HasPrefix(params.trunkurl, "rtsp") {
			c := &RtspClient{
				BaseClient: BaseClient{
					label:  fmt.Sprintf("RTSP client of %s", params.trunkurl),
					logger: params.logger,
				},

				url:      params.trunkurl,
				observer: params.observer,
			}

			c.run(params.s)

			return c
		} else {
			c := &client{
				BaseClient: BaseClient{
					label:  fmt.Sprintf("Trunking client to %s", params.trunkurl),
					logger: params.logger,
				},

				trunkurl: params.trunkurl,
				observer: params.observer,
			}

			c.run(nil, params.s)

			return c
		}
	}
}
