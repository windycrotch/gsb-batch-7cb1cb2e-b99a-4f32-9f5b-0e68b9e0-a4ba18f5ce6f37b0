package sfu

import (
	"time"

	"atomirex.com/umbrella/razor"
)

// newSfuHandlerCallback builds the SFU command handler. It is separated from NewSfu so
// that tests can construct an Sfu with a fake client factory and no WebRTC media stack.
func (s *Sfu) newSfuHandlerCallback() razor.MessageHandlerCallback[sfuCommand, sfuCommandMessage] {
	return func(what sfuCommand, payload *sfuCommandMessage) bool {
		shouldSignalClients := false

		switch what {
		case sfuAddClient:
			added := payload.remoteClient
			if added == nil {
				added = payload.client
			}

			s.clients = append(s.clients, added)

			// Add all existing tracks
			for _, t := range s.localTracks {
				added.AddOutgoingTracksForIncomingTrack(t)
			}

			shouldSignalClients = true
		case sfuRemoveClient:
			removed := payload.remoteClient
			if removed == nil {
				removed = payload.client
			}

			// Removing a client also drops any tracks it contributed. This is
			// idempotent with the explicit sfuRemoveClientTracks message, and makes it
			// impossible for a departed session's tracks to outlive it even if the
			// client teardown path is cut short.
			if s.removeTracksOwnedByLocked(removed) {
				shouldSignalClients = true
			}

			index := -1
			for i, c := range s.clients {
				if c == removed {
					index = i
				}
			}

			if index >= 0 {
				s.clients = append(s.clients[:index], s.clients[index+1:]...)
			}

			shouldSignalClients = true
		case sfuRemoveClientTracks:
			owner := payload.remoteClient
			if owner == nil {
				owner = payload.client
			}

			if s.removeTracksOwnedByLocked(owner) {
				shouldSignalClients = true
			}
		case sfuAddOutgoingTracksForIncomingTrack:
			s.logger.Info("sfu", "adding track: "+payload.intrack.String())
			intrack := payload.intrack
			s.localTracks[intrack.UmbrellaID()] = intrack

			if payload.trackOwner != nil {
				s.trackOwners[intrack.UmbrellaID()] = payload.trackOwner
			}

			for _, c := range s.clients {
				c.AddOutgoingTracksForIncomingTrack(intrack)
			}

			shouldSignalClients = true
		case sfuRemoveAllOutgoingTracksForIncomingTrack:
			s.logger.Info("sfu", "removing all outgoing tracks for track: "+payload.intrack.String())
			s.removeLocalTrackLocked(payload.intrack)

			shouldSignalClients = true
		case sfuSignalClients:
			shouldSignalClients = true
		case sfuGetStatus:
			// Terrible version of this which blocks while polling everything
			// Should really pass around a thing that gathers the info
			// but in reality I should move to a pub/sub thing that pushes the data out on changes anyway
			// this is to debug the mess as is :P
			s.logger.Info("sfu", "SFU getting status relaying")
			relaying := make([]*TrackDescriptor, 0)
			for _, t := range s.localTracks {
				relaying = append(relaying, t.descriptor)
			}
			s.logger.Info("sfu", "SFU getting status clients")
			clients := make([]*SFUStatusClient, 0)
			for _, c := range s.clients {
				// A client which is wedged must not block /status for everyone else.
				status := c.getStatusWithTimeout(2 * time.Second)
				if status == nil {
					continue
				}
				clients = append(clients, status)
			}

			serve := s.snapshotServers()

			s.logger.Info("sfu", "SFU getting status returning")
			status := &SFUStatus{
				RelayingTracks: relaying,
				Servers:        serve.Servers,
				ServerStatuses: serve.ServerStatuses,
				Clients:        clients,
			}

			payload.result.status <- status
		case sfuGetCurrentServers:
			payload.result.servers <- s.snapshotServers()
		case sfuSetCurrentServers:
			// "intended" servers model, then regularly evaluate, so setup/teardown repeatedly is ok
			mentioned := make(map[string]bool)
			for _, t := range payload.SetCurrentServers.Servers {
				if t == "" {
					continue
				}
				// Duplicates collapse here, so the same address can never produce
				// more than one connection.
				mentioned[t] = true
			}

			// Just overwrite it with what the client sent! (For now)
			s.intendedServers = mentioned

			s.evaluateServers()

			shouldSignalClients = true

			payload.result.servers <- s.snapshotServers()
		case sfuServerConnectionEvent:
			event := payload.serverEvent

			wasConnected := s.isServerConnected(event.url)

			s.handleServerConnectionEvent(event)

			if wasConnected != s.isServerConnected(event.url) {
				shouldSignalClients = true
			}
		case sfuServerRetry:
			s.handleServerRetry(payload.serverURL, payload.serverGeneration)
		case sfuServerConnectTimeout:
			s.handleServerConnectTimeout(payload.serverURL, payload.serverGeneration)
		}

		if shouldSignalClients {
			s.logger.Verbose("sfu", "SFU should signaling clients")
			s.handler.Cancel(sfuSignalClients)

			for _, c := range s.clients {
				s.logger.Verbose("sfu", "SFU signaling client "+c.Label())
				c.RequestEvalState()
			}

			s.logger.Verbose("sfu", "SFU finished signaling clients")
		}

		return true
	}
}

// NewSfuWithFactory builds an Sfu which creates remote clients through the supplied
// factory and has no WebRTC peer connection factory. It is intended for tests and for
// embedders which need to control how outbound relay connections are made.
func NewSfuWithFactory(logger *razor.Logger, factory RemoteClientFactory) *Sfu {
	s := &Sfu{
		sfuCommands:         make(chan sfuCommandMessage, 256),
		remoteClientFactory: factory,
		localTracks:         make(map[string]*incomingTrack),
		trackOwners:         make(map[string]RemoteClient),
		intendedServers:     make(map[string]bool),
		servers:             make(map[string]*serverEntry),
		logger:              logger,

		serverRetryBaseDelay:     defaultServerRetryBaseDelay,
		serverRetryMaxDelay:      defaultServerRetryMaxDelay,
		serverMaxConnectAttempts: defaultServerMaxConnectAttempts,
		serverConnectTimeout:     defaultServerConnectTimeout,

		serverHealthyConnectionDuration: defaultServerHealthyConnectionDuration,
	}

	s.handler = razor.NewMessageHandler(logger, "sfu", 1024, s.newSfuHandlerCallback())
	s.handler.Loop(func() {
		panic("SFU unexpectedly terminated")
	})

	return s
}
