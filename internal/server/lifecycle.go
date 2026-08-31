package server

import (
	"time"

	"go.uber.org/zap"
)

func (s *Server) cleanupExpiredSessions() {
	ticker := time.NewTicker(SessionCleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		s.cleanupExpiredSessionsOnce(time.Now())
	}
}

func (s *Server) cleanupExpiredSessionsOnce(now time.Time) {
	minRetentionTime := s.startTime.Add(MinRoomRetentionAfterRestart)

	// First, determine which sessions have expired without holding any room locks.
	s.mu.Lock()
	expired := make([]*Session, 0)
	for token, session := range s.sessions {
		if session == nil {
			delete(s.sessions, token)
			continue
		}
		if now.Sub(session.DisconnectAt) > ReconnectGracePeriod {
			expired = append(expired, session)
			delete(s.sessions, token)
			s.logger.Info("Session expired",
				zap.String("user_id", session.UserID),
				zap.String("room_code", session.RoomCode))
		}
	}
	s.mu.Unlock()

	// Now process the side effects for each expired session without
	// ever taking the server lock and a room lock at the same time.
	for _, session := range expired {
		s.mu.RLock()
		room, exists := s.rooms[session.RoomCode]
		s.mu.RUnlock()
		if !exists || room == nil {
			continue
		}

		room.mu.Lock()

		delete(room.DisconnectedUsers, session.UserID)
		expiredWasHost := session.IsHost || room.State.HostID == session.UserID

		// Remove from room state users if still there
		newUsers := make([]UserInfo, 0, len(room.State.Users))
		for _, u := range room.State.Users {
			if u.UserID != session.UserID {
				newUsers = append(newUsers, u)
			}
		}
		room.State.Users = newUsers

		var hostChanged *HostChangedPayload
		if expiredWasHost {
			var newHost *Client
			for _, client := range room.Clients {
				if client != nil {
					newHost = client
					break
				}
			}
			room.Host = newHost
			room.HostDisconnectedAt = nil
			if newHost != nil {
				newHostID := newHost.clientID()
				room.State.HostID = newHostID
				for i := range room.State.Users {
					room.State.Users[i].IsHost = room.State.Users[i].UserID == newHostID
				}
				hostChanged = &HostChangedPayload{
					NewHostID:   newHostID,
					NewHostName: newHost.userName(),
				}
			} else {
				room.State.HostID = ""
			}
		}

		// Capture information needed after releasing the room lock
		shouldDeleteRoom := len(room.Clients) == 0 && len(room.DisconnectedUsers) == 0 && now.After(minRetentionTime)
		roomCode := room.Code
		remainingClients := make([]*Client, 0, len(room.Clients))
		for _, client := range room.Clients {
			if client != nil {
				remainingClients = append(remainingClients, client)
			}
		}

		room.mu.Unlock()

		// If the room is now empty and past the retention window, delete it.
		if shouldDeleteRoom {
			if s.deleteRoomIfEmpty(room) {
				s.logger.Info("Deleted empty room",
					zap.String("room_code", roomCode))
			}
			continue
		}

		// Notify remaining users that the expired session permanently left.
		for _, client := range remainingClients {
			client.sendMessage(s.logger, MsgTypeUserLeft, UserLeftPayload{
				UserID:   session.UserID,
				Username: session.Username,
			})
			if hostChanged != nil {
				client.sendMessage(s.logger, MsgTypeHostChanged, *hostChanged)
			}
		}
	}
}

func (s *Server) cleanupEmptyRooms() {
	// Wait 5 minutes before first cleanup to avoid deleting rooms during startup
	time.Sleep(EmptyRoomCleanupTimeout)

	ticker := time.NewTicker(EmptyRoomCleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		minRetentionTime := s.startTime.Add(MinRoomRetentionAfterRestart)

		s.mu.RLock()
		rooms := make(map[string]*Room, len(s.rooms))
		for roomCode, room := range s.rooms {
			rooms[roomCode] = room
		}
		s.mu.RUnlock()

		for roomCode, room := range rooms {
			if room == nil {
				continue
			}

			room.mu.RLock()
			isEmpty := len(room.Clients) == 0 && len(room.DisconnectedUsers) == 0
			emptySince := room.EmptySince
			room.mu.RUnlock()

			if !isEmpty {
				continue
			}

			// If room just became empty, mark it
			if emptySince == nil {
				room.mu.Lock()
				nowPtr := now
				room.EmptySince = &nowPtr
				room.mu.Unlock()
				s.logger.Info("Room became empty, scheduling cleanup",
					zap.String("room_code", roomCode))
				continue
			}

			// Check if room has been empty long enough and past retention window
			if now.Sub(*emptySince) > EmptyRoomCleanupTimeout && now.After(minRetentionTime) {
				if s.deleteRoomIfEmpty(room) {
					s.logger.Info("Deleted empty room after inactivity",
						zap.String("room_code", roomCode),
						zap.Duration("empty_for", now.Sub(*emptySince)))
				}
			}
		}
	}
}

func (s *Server) removeClient(c *Client) {
	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
	c.closeSend()

	if c.currentRoom() != nil {
		s.handleClientDisconnect(c)
	} else {
		s.removePendingJoin(c)
		if c.currentRoom() != nil {
			s.handleClientDisconnect(c)
		}
	}

	s.logger.Info("Client disconnected", zap.String("client_id", c.clientID()))
}

func (s *Server) removePendingJoin(c *Client) {
	clientID := c.clientID()
	if clientID == "" {
		return
	}

	s.mu.RLock()
	rooms := make([]*Room, 0, len(s.rooms))
	for _, room := range s.rooms {
		rooms = append(rooms, room)
	}
	s.mu.RUnlock()

	for _, room := range rooms {
		room.mu.Lock()
		if pending, exists := room.PendingJoins[clientID]; exists && pending == c {
			delete(room.PendingJoins, clientID)
			c.clearPendingRoom(room)
		}
		room.mu.Unlock()
	}
}

// handleClientDisconnect handles a client disconnecting - creates a session for reconnection
func (s *Server) handleClientDisconnect(c *Client) {
	room := c.currentRoom()
	if room == nil {
		return
	}

	clientID := c.clientID()
	username := c.userName()
	sessionToken := c.session()
	if sessionToken == "" {
		sessionToken = s.generateSessionToken()
		c.setSessionToken(sessionToken)
	}

	s.mu.Lock()
	room.mu.Lock()
	if room.Clients[clientID] != c {
		room.mu.Unlock()
		s.mu.Unlock()
		c.clearRoom(room)
		return
	}

	wasHost := room.Host == c

	// Create session for reconnection
	session := &Session{
		UserID:       clientID,
		Username:     username,
		RoomCode:     room.Code,
		IsHost:       wasHost,
		DisconnectAt: time.Now(),
	}

	// Remove from active clients but add to disconnected users
	delete(room.Clients, clientID)
	if room.BufferingUsers != nil {
		delete(room.BufferingUsers, clientID)
	}

	if room.DisconnectedUsers == nil {
		room.DisconnectedUsers = make(map[string]*Session)
	}
	room.DisconnectedUsers[clientID] = session

	// Mark user as disconnected in room state
	for i := range room.State.Users {
		if room.State.Users[i].UserID == clientID {
			room.State.Users[i].IsConnected = false
			break
		}
	}

	// Track if host disconnected
	if wasHost {
		now := time.Now()
		room.HostDisconnectedAt = &now
	}

	c.clearRoom(room)

	// Collect clients to notify before unlocking
	clientsToNotify := make([]*Client, 0, len(room.Clients))
	for _, client := range room.Clients {
		if client != nil {
			clientsToNotify = append(clientsToNotify, client)
		}
	}

	room.EmptySince = nil
	s.sessions[sessionToken] = session
	room.mu.Unlock()
	s.mu.Unlock()

	// Notify other users about the temporary disconnect
	for _, client := range clientsToNotify {
		client.sendMessage(s.logger, MsgTypeUserDisconnected, UserDisconnectedPayload{
			UserID:   clientID,
			Username: username,
		})
	}

	s.logger.Info("User temporarily disconnected",
		zap.String("username", username),
		zap.String("user_id", clientID),
		zap.String("room_code", room.Code),
		zap.Bool("was_host", wasHost))
}

// handleReconnect handles a client trying to reconnect to their room
func (s *Server) handleReconnect(c *Client, payload []byte) {
	var p ReconnectPayload
	if err := decodePayload(payload, MsgTypeReconnect, &p); err != nil {
		c.sendError(s.logger, "invalid_payload", "Invalid reconnect payload")
		return
	}

	if p.SessionToken == "" {
		c.sendError(s.logger, "missing_session_token", "Session token is required")
		return
	}
	if c.currentRoom() != nil {
		c.sendError(s.logger, "already_in_room", "Leave the current room before reconnecting")
		return
	}
	if c.currentPendingRoom() != nil {
		c.sendError(s.logger, "already_pending", "Cancel the pending join request before reconnecting")
		return
	}

	now := time.Now()
	s.mu.Lock()
	session, exists := s.sessions[p.SessionToken]
	expired := exists && (session == nil || now.Sub(session.DisconnectAt) > ReconnectGracePeriod)
	if exists {
		delete(s.sessions, p.SessionToken)
	}
	s.mu.Unlock()

	if !exists {
		c.sendError(s.logger, "session_not_found", "Session not found or expired")
		return
	}

	// Check if session is expired
	if expired {
		c.sendError(s.logger, "session_expired", "Session has expired")
		return
	}

	s.mu.RLock()
	room, roomExists := s.rooms[session.RoomCode]
	s.mu.RUnlock()

	if !roomExists {
		s.mu.Lock()
		delete(s.sessions, p.SessionToken)
		s.mu.Unlock()
		c.sendError(s.logger, "room_not_found", "Room no longer exists")
		return
	}

	room.syncMu.Lock()
	room.mu.Lock()
	disconnectedSession, disconnected := room.DisconnectedUsers[session.UserID]
	if !disconnected || disconnectedSession == nil || room.Clients[session.UserID] != nil {
		room.mu.Unlock()
		room.syncMu.Unlock()
		c.sendError(s.logger, "session_not_found", "Session is no longer reconnectable")
		return
	}
	if !c.trySetRoom(room) {
		room.mu.Unlock()
		room.syncMu.Unlock()
		c.sendError(s.logger, "already_in_room", "Leave the current room before reconnecting")
		return
	}

	// Restore the client
	c.setClientID(session.UserID)
	c.setUsername(session.Username)
	c.setSessionToken(p.SessionToken)

	// Add back to room clients
	room.Clients[session.UserID] = c
	delete(room.DisconnectedUsers, session.UserID)
	room.EmptySince = nil

	// Mark user as connected in room state
	for i := range room.State.Users {
		if room.State.Users[i].UserID == session.UserID {
			room.State.Users[i].IsConnected = true
			break
		}
	}

	// Restore host status if they were the host
	if session.IsHost || (room.Host == nil && room.State.HostID == "") {
		room.Host = c
		room.HostDisconnectedAt = nil
		room.State.HostID = session.UserID

		// Update IsHost flag in users list
		for i := range room.State.Users {
			room.State.Users[i].IsHost = room.State.Users[i].UserID == session.UserID
		}
	}

	// Calculate live position for reconnect state
	nowMs := time.Now().UnixMilli()
	liveState := cloneLiveRoomState(room.State, nowMs)

	isHost := room.Host == c
	pendingJoinRequests := make([]JoinRequestPayload, 0, len(room.PendingJoins))
	pendingSuggestions := make([]SuggestionReceivedPayload, 0, len(room.PendingSuggestions))
	if isHost {
		for _, pendingClient := range room.PendingJoins {
			if pendingClient == nil || pendingClient.isClosed() {
				continue
			}
			pendingJoinRequests = append(pendingJoinRequests, JoinRequestPayload{
				UserID:   pendingClient.clientID(),
				Username: pendingClient.userName(),
			})
		}
		for _, suggestion := range room.PendingSuggestions {
			if suggestion == nil {
				continue
			}
			pendingSuggestions = append(pendingSuggestions, SuggestionReceivedPayload{
				SuggestionID: suggestion.ID,
				FromUserID:   suggestion.FromUserID,
				FromUsername: suggestion.FromUsername,
				TrackInfo:    cloneTrackInfo(suggestion.Track),
			})
		}
	}

	clientsToNotify := make([]*Client, 0, len(room.Clients))
	for _, client := range room.Clients {
		if client != nil && client != c {
			clientsToNotify = append(clientsToNotify, client)
		}
	}

	room.mu.Unlock()

	// Send reconnected message to the client with LIVE state
	c.sendMessage(s.logger, MsgTypeReconnected, ReconnectedPayload{
		RoomCode: room.Code,
		UserID:   c.clientID(),
		State:    liveState,
		IsHost:   isHost,
	})
	room.syncMu.Unlock()

	if isHost {
		for _, joinRequest := range pendingJoinRequests {
			c.sendMessage(s.logger, MsgTypeJoinRequest, joinRequest)
		}
		for _, suggestion := range pendingSuggestions {
			c.sendMessage(s.logger, MsgTypeSuggestionReceived, suggestion)
		}

		if len(pendingJoinRequests) > 0 {
			s.logger.Info("Replayed pending join requests to reconnected host",
				zap.String("host_id", c.clientID()),
				zap.String("room_code", room.Code),
				zap.Int("pending_count", len(pendingJoinRequests)))
		}
	}

	// Notify other users
	for _, client := range clientsToNotify {
		client.sendMessage(s.logger, MsgTypeUserReconnected, UserReconnectedPayload{
			UserID:   c.clientID(),
			Username: c.userName(),
		})
	}

	s.logger.Info("User reconnected",
		zap.String("username", c.userName()),
		zap.String("user_id", c.clientID()),
		zap.String("room_code", room.Code),
		zap.Bool("is_host", isHost))
}

func (s *Server) deleteRoomIfEmpty(room *Room) bool {
	s.mu.Lock()
	currentRoom, exists := s.rooms[room.Code]
	if !exists || currentRoom != room {
		s.mu.Unlock()
		return false
	}

	room.mu.Lock()
	if len(room.Clients) != 0 || len(room.DisconnectedUsers) != 0 {
		room.mu.Unlock()
		s.mu.Unlock()
		return false
	}

	pendingClients := make([]*Client, 0, len(room.PendingJoins))
	for _, client := range room.PendingJoins {
		if client != nil {
			client.clearPendingRoom(room)
			pendingClients = append(pendingClients, client)
		}
	}
	room.PendingJoins = make(map[string]*Client)
	delete(s.rooms, room.Code)
	room.mu.Unlock()
	s.mu.Unlock()

	for _, client := range pendingClients {
		client.sendMessage(s.logger, MsgTypeJoinRejected, JoinRejectedPayload{Reason: "Room is no longer available"})
	}
	return true
}
