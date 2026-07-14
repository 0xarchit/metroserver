package server

import (
	"fmt"
	"time"

	"go.uber.org/zap"
)

const maxActionTransitCompensation = 5 * time.Second

func roomClientsLocked(room *Room) []*Client {
	clients := make([]*Client, 0, len(room.Clients))
	for _, client := range room.Clients {
		if client != nil {
			clients = append(clients, client)
		}
	}
	return clients
}

func validatePlaybackTrack(c *Client, logger *zap.Logger, state *RoomState, p *PlaybackActionPayload) bool {
	if state.CurrentTrack == nil {
		return true
	}
	if p.TrackID == "" {
		p.TrackID = state.CurrentTrack.ID
		return true
	}
	if p.TrackID != state.CurrentTrack.ID {
		c.sendError(logger, "stale_track", "Playback action targets a stale track")
		return false
	}
	return true
}

func clampPlaybackPosition(position int64, track *TrackInfo) int64 {
	if position < 0 {
		return 0
	}
	if track != nil && track.Duration > 0 && position > track.Duration {
		return track.Duration
	}
	return position
}

func compensateActionTransit(position, capturedAtServerTime, nowMs int64) int64 {
	if capturedAtServerTime <= 0 || capturedAtServerTime >= nowMs {
		return position
	}
	elapsed := nowMs - capturedAtServerTime
	if elapsed > maxActionTransitCompensation.Milliseconds() {
		elapsed = maxActionTransitCompensation.Milliseconds()
	}
	return position + elapsed
}

func sanitizeQueue(queue []TrackInfo) []TrackInfo {
	sanitized := make([]TrackInfo, 0, len(queue))
	seen := make(map[string]struct{}, len(queue))
	for _, track := range queue {
		if !sanitizeTrackInfo(&track) {
			continue
		}
		if _, exists := seen[track.ID]; exists {
			continue
		}
		seen[track.ID] = struct{}{}
		sanitized = append(sanitized, track)
		if len(sanitized) == MaxQueueSize {
			break
		}
	}
	return sanitized
}

func sanitizeUpcomingQueue(queue []TrackInfo, currentTrackID string) []TrackInfo {
	if currentTrackID == "" {
		return sanitizeQueue(queue)
	}
	upcoming := make([]TrackInfo, 0, len(queue))
	for i := range queue {
		if queue[i].ID != currentTrackID {
			upcoming = append(upcoming, queue[i])
		}
	}
	return sanitizeQueue(upcoming)
}

func containsTrackID(queue []TrackInfo, trackID string) bool {
	for i := range queue {
		if queue[i].ID == trackID {
			return true
		}
	}
	return false
}

func (s *Server) handlePlaybackAction(c *Client, payload []byte) {
	var p PlaybackActionPayload
	if err := decodePayload(payload, MsgTypePlaybackAction, &p); err != nil {
		c.sendError(s.logger, "invalid_payload", "Invalid playback action payload")
		return
	}
	if p.Action == "" {
		c.sendError(s.logger, "missing_action", "Action is required")
		return
	}

	room := c.currentRoom()
	if room == nil {
		c.sendError(s.logger, "not_in_room", "You are not in a room")
		return
	}

	room.syncMu.Lock()
	defer room.syncMu.Unlock()
	room.mu.Lock()

	if room.Host == nil || room.Host != c || room.HostDisconnectedAt != nil {
		room.mu.Unlock()
		c.sendError(s.logger, "not_host", "Only the host can control playback")
		return
	}

	nowMs := time.Now().UnixMilli()
	switch p.Action {
	case ActionPlay:
		if room.State.CurrentTrack == nil {
			room.mu.Unlock()
			c.sendError(s.logger, "no_track", "Cannot play without a track")
			return
		}
		if p.Position < 0 {
			room.mu.Unlock()
			c.sendError(s.logger, "invalid_position", "Position cannot be negative")
			return
		}
		if !validatePlaybackTrack(c, s.logger, room.State, &p) {
			room.mu.Unlock()
			return
		}
		p.Position = compensateActionTransit(p.Position, p.CapturedAtServerTime, nowMs)
		p.Position = clampPlaybackPosition(p.Position, room.State.CurrentTrack)
		room.State.IsPlaying = true
		room.State.Position = p.Position
		room.State.LastUpdate = nowMs

	case ActionPause:
		if p.Position < 0 {
			room.mu.Unlock()
			c.sendError(s.logger, "invalid_position", "Position cannot be negative")
			return
		}
		if !validatePlaybackTrack(c, s.logger, room.State, &p) {
			room.mu.Unlock()
			return
		}
		p.Position = clampPlaybackPosition(p.Position, room.State.CurrentTrack)
		room.State.IsPlaying = false
		room.State.Position = p.Position
		room.State.LastUpdate = nowMs

	case ActionSeek:
		if p.Position < 0 {
			room.mu.Unlock()
			c.sendError(s.logger, "invalid_position", "Position cannot be negative")
			return
		}
		if !validatePlaybackTrack(c, s.logger, room.State, &p) {
			room.mu.Unlock()
			return
		}
		if room.State.IsPlaying {
			p.Position = compensateActionTransit(p.Position, p.CapturedAtServerTime, nowMs)
		}
		p.Position = clampPlaybackPosition(p.Position, room.State.CurrentTrack)
		room.State.Position = p.Position
		room.State.LastUpdate = nowMs

	case ActionChangeTrack:
		if p.TrackInfo == nil {
			room.mu.Unlock()
			c.sendError(s.logger, "missing_track_info", "Track info is required for track change")
			return
		}
		if !sanitizeTrackInfo(p.TrackInfo) {
			room.mu.Unlock()
			c.sendError(s.logger, "invalid_track_info", "Track must have ID and title")
			return
		}
		if p.Queue != nil {
			p.Queue = sanitizeUpcomingQueue(p.Queue, p.TrackInfo.ID)
			room.State.Queue = append([]TrackInfo(nil), p.Queue...)
		}
		room.State.CurrentTrack = cloneTrackInfo(p.TrackInfo)
		room.State.Queue = sanitizeUpcomingQueue(room.State.Queue, p.TrackInfo.ID)
		room.State.Position = 0
		room.State.IsPlaying = false
		room.State.LastUpdate = nowMs
		room.HostStartPosition = 0
		room.BufferingUsers = nil
		p.TrackID = p.TrackInfo.ID
		p.Position = 0
		s.logger.Debug("Track changed", zap.String("room_code", room.Code), zap.String("track_id", p.TrackInfo.ID))

	case ActionSkipNext, ActionSkipPrev:
		// The host's media transition produces the canonical change_track event.
		room.mu.Unlock()
		return

	case ActionQueueAdd:
		if p.TrackInfo == nil {
			room.mu.Unlock()
			c.sendError(s.logger, "missing_track_info", "Track info is required for queue add")
			return
		}
		if !sanitizeTrackInfo(p.TrackInfo) {
			room.mu.Unlock()
			c.sendError(s.logger, "invalid_track_info", "Track must have ID and title")
			return
		}
		if len(room.State.Queue) >= MaxQueueSize {
			room.mu.Unlock()
			c.sendError(s.logger, "queue_full", "Queue is full")
			return
		}
		if (room.State.CurrentTrack != nil && room.State.CurrentTrack.ID == p.TrackInfo.ID) ||
			containsTrackID(room.State.Queue, p.TrackInfo.ID) {
			room.mu.Unlock()
			c.sendError(s.logger, "duplicate_track", "Track is already playing or queued")
			return
		}
		if p.InsertNext {
			room.State.Queue = append([]TrackInfo{*p.TrackInfo}, room.State.Queue...)
		} else {
			room.State.Queue = append(room.State.Queue, *p.TrackInfo)
		}

	case ActionQueueRemove:
		if p.TrackID == "" {
			room.mu.Unlock()
			c.sendError(s.logger, "missing_track_id", "Track ID is required for queue remove")
			return
		}
		queue := make([]TrackInfo, 0, len(room.State.Queue))
		for _, track := range room.State.Queue {
			if track.ID != p.TrackID {
				queue = append(queue, track)
			}
		}
		room.State.Queue = queue

	case ActionQueueClear:
		room.State.Queue = []TrackInfo{}

	case ActionSyncQueue:
		currentTrackID := ""
		if room.State.CurrentTrack != nil {
			currentTrackID = room.State.CurrentTrack.ID
		}
		p.Queue = sanitizeUpcomingQueue(p.Queue, currentTrackID)
		room.State.Queue = append([]TrackInfo(nil), p.Queue...)

	case ActionSetVolume:
		if p.Volume < 0 || p.Volume > 1 {
			room.mu.Unlock()
			c.sendError(s.logger, "invalid_volume", "Volume must be between 0 and 1")
			return
		}
		room.State.Volume = p.Volume

	default:
		room.mu.Unlock()
		c.sendError(s.logger, "unknown_action", fmt.Sprintf("Unknown action: %s", p.Action))
		return
	}
	if p.Action == ActionChangeTrack || p.Action == ActionQueueAdd || p.Action == ActionQueueRemove ||
		p.Action == ActionQueueClear || p.Action == ActionSyncQueue {
		p.Queue = append([]TrackInfo(nil), room.State.Queue...)
	}

	room.State.Revision++
	p.Revision = room.State.Revision
	p.ServerTime = nowMs
	clients := roomClientsLocked(room)
	room.mu.Unlock()

	sendMessageToClients(s.logger, clients, MsgTypeSyncPlayback, p)
	s.logger.Debug("Playback action processed",
		zap.String("action", p.Action),
		zap.String("room_code", room.Code),
		zap.String("host_name", c.userName()),
		zap.Uint64("revision", p.Revision))
}

func (s *Server) handleBufferReady(c *Client, payload []byte) {
	var p BufferReadyPayload
	if err := decodePayload(payload, MsgTypeBufferReady, &p); err != nil {
		c.sendError(s.logger, "invalid_payload", "Invalid buffer ready payload")
		return
	}
	if p.TrackID == "" {
		c.sendError(s.logger, "missing_track_id", "Track ID is required")
		return
	}

	room := c.currentRoom()
	if room == nil {
		c.sendError(s.logger, "not_in_room", "You are not in a room")
		return
	}

	room.syncMu.Lock()
	defer room.syncMu.Unlock()
	room.mu.Lock()
	clientID := c.clientID()
	if room.Clients[clientID] != c {
		room.mu.Unlock()
		c.sendError(s.logger, "not_in_room", "You are not an active room member")
		return
	}
	if room.State.CurrentTrack == nil || p.TrackID != room.State.CurrentTrack.ID {
		room.mu.Unlock()
		c.sendError(s.logger, "stale_track", "Buffer readiness targets a stale track")
		return
	}

	coordinatedBuffering := room.BufferingUsers != nil
	if coordinatedBuffering {
		delete(room.BufferingUsers, clientID)
		if len(room.BufferingUsers) > 0 {
			waitingFor := make([]string, 0, len(room.BufferingUsers))
			for id := range room.BufferingUsers {
				waitingFor = append(waitingFor, id)
			}
			clients := roomClientsLocked(room)
			room.mu.Unlock()
			sendMessageToClients(s.logger, clients, MsgTypeBufferWait, BufferWaitPayload{TrackID: p.TrackID, WaitingFor: waitingFor})
			return
		}
		room.BufferingUsers = nil
	}

	nowMs := time.Now().UnixMilli()
	position := livePlaybackPosition(room.State, nowMs)
	trackID := room.State.CurrentTrack.ID
	playing := room.State.IsPlaying
	revision := room.State.Revision
	clients := []*Client{c}
	if coordinatedBuffering {
		clients = roomClientsLocked(room)
	}
	room.mu.Unlock()

	sendMessageToClients(s.logger, clients, MsgTypeSyncPlayback, PlaybackActionPayload{
		Action: ActionSeek, TrackID: trackID, Position: position, ServerTime: nowMs, Revision: revision,
	})
	stateAction := ActionPause
	if playing {
		stateAction = ActionPlay
	}
	sendMessageToClients(s.logger, clients, MsgTypeSyncPlayback, PlaybackActionPayload{
		Action: stateAction, TrackID: trackID, Position: position, ServerTime: nowMs, Revision: revision,
	})
	sendMessageToClients(s.logger, clients, MsgTypeBufferComplete, BufferCompletePayload{TrackID: trackID})
}

func (s *Server) handleRequestSync(c *Client) {
	room := c.currentRoom()
	if room == nil {
		c.sendError(s.logger, "not_in_room", "You are not in a room")
		return
	}

	room.syncMu.Lock()
	defer room.syncMu.Unlock()
	room.mu.RLock()
	nowMs := time.Now().UnixMilli()
	position := livePlaybackPosition(room.State, nowMs)
	response := SyncStatePayload{
		CurrentTrack: cloneTrackInfo(room.State.CurrentTrack),
		IsPlaying:    room.State.IsPlaying,
		Position:     position,
		LastUpdate:   nowMs,
		Queue:        append([]TrackInfo(nil), room.State.Queue...),
		Volume:       room.State.Volume,
		Revision:     room.State.Revision,
	}
	room.mu.RUnlock()

	s.logger.Debug("Sync request received",
		zap.String("username", c.userName()),
		zap.String("user_id", c.clientID()),
		zap.Bool("response_playing", response.IsPlaying),
		zap.Int64("position", position),
		zap.Uint64("revision", response.Revision))
	c.sendMessage(s.logger, MsgTypeSyncState, response)
}
