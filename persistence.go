package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"
)

const StateFile = "server_state.json"

// PersistentState contains all data that needs to be saved across server restarts
type PersistentState struct {
	ServerShutdownTime time.Time           `json:"server_shutdown_time"`
	Rooms              []PersistentRoom    `json:"rooms"`
	Sessions           []PersistentSession `json:"sessions"`
}

// PersistentRoom is a serializable version of Room
type PersistentRoom struct {
	Code               string                 `json:"code"`
	HostID             string                 `json:"host_id"`
	State              *RoomState             `json:"state"`
	DisconnectedUsers  map[string]*Session    `json:"disconnected_users"`
	PendingSuggestions []PersistentSuggestion `json:"pending_suggestions"`
	HostDisconnectedAt *time.Time             `json:"host_disconnected_at,omitempty"`
}

// PersistentSuggestion is a serializable version of Suggestion
type PersistentSuggestion struct {
	ID           string     `json:"id"`
	FromUserID   string     `json:"from_user_id"`
	FromUsername string     `json:"from_username"`
	Track        *TrackInfo `json:"track"`
}

// PersistentSession is a serializable version of Session with token
type PersistentSession struct {
	Token        string    `json:"token"`
	UserID       string    `json:"user_id"`
	Username     string    `json:"username"`
	RoomCode     string    `json:"room_code"`
	IsHost       bool      `json:"is_host"`
	DisconnectAt time.Time `json:"disconnect_at"`
}

// SaveState saves the current server state to disk
func (s *Server) SaveState() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	state := PersistentState{
		ServerShutdownTime: time.Now(),
		Rooms:              make([]PersistentRoom, 0),
		Sessions:           make([]PersistentSession, 0),
	}

	// Save all rooms
	for _, room := range s.rooms {
		room.mu.RLock()

		// Convert pending suggestions
		pendingSuggestions := make([]PersistentSuggestion, 0, len(room.PendingSuggestions))
		for _, suggestion := range room.PendingSuggestions {
			pendingSuggestions = append(pendingSuggestions, PersistentSuggestion{
				ID:           suggestion.ID,
				FromUserID:   suggestion.FromUserID,
				FromUsername: suggestion.FromUsername,
				Track:        suggestion.Track,
			})
		}

		persistentRoom := PersistentRoom{
			Code:               room.Code,
			HostID:             room.Host.ID,
			State:              room.State,
			DisconnectedUsers:  room.DisconnectedUsers,
			PendingSuggestions: pendingSuggestions,
			HostDisconnectedAt: room.HostDisconnectedAt,
		}

		state.Rooms = append(state.Rooms, persistentRoom)
		room.mu.RUnlock()
	}

	// Save all sessions
	for token, session := range s.sessions {
		state.Sessions = append(state.Sessions, PersistentSession{
			Token:        token,
			UserID:       session.UserID,
			Username:     session.Username,
			RoomCode:     session.RoomCode,
			IsHost:       session.IsHost,
			DisconnectAt: session.DisconnectAt,
		})
	}

	// Marshal to JSON
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	// Write to file
	if err := os.WriteFile(StateFile, data, 0644); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}

	s.logger.Info("Server state saved",
		zap.Int("rooms", len(state.Rooms)),
		zap.Int("sessions", len(state.Sessions)))

	return nil
}

// LoadState loads the server state from disk
func (s *Server) LoadState() error {
	// Check if state file exists
	if _, err := os.Stat(StateFile); os.IsNotExist(err) {
		s.logger.Info("No previous state file found, starting fresh")
		return nil
	}

	// Read state file
	data, err := os.ReadFile(StateFile)
	if err != nil {
		return fmt.Errorf("read state file: %w", err)
	}

	var state PersistentState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("unmarshal state: %w", err)
	}

	// Calculate time elapsed since shutdown
	shutdownDuration := time.Since(state.ServerShutdownTime)
	s.logger.Info("Loading previous state",
		zap.Duration("offline_duration", shutdownDuration),
		zap.Int("rooms", len(state.Rooms)),
		zap.Int("sessions", len(state.Sessions)))

	// Restore rooms
	for _, persistentRoom := range state.Rooms {
		room := &Room{
			Code:               persistentRoom.Code,
			Host:               nil, // Will be nil initially - user needs to reconnect
			Clients:            make(map[string]*Client),
			PendingJoins:       make(map[string]*Client),
			PendingSuggestions: make(map[string]*Suggestion),
			DisconnectedUsers:  persistentRoom.DisconnectedUsers,
			State:              persistentRoom.State,
			BufferingUsers:     make(map[string]bool),
			HostDisconnectedAt: persistentRoom.HostDisconnectedAt,
		}

		// Restore pending suggestions
		for _, ps := range persistentRoom.PendingSuggestions {
			room.PendingSuggestions[ps.ID] = &Suggestion{
				ID:           ps.ID,
				FromUserID:   ps.FromUserID,
				FromUsername: ps.FromUsername,
				Track:        ps.Track,
			}
		}

		// Update disconnect times for all users to account for shutdown duration
		for _, session := range room.DisconnectedUsers {
			session.DisconnectAt = session.DisconnectAt.Add(shutdownDuration)
		}

		// Update host disconnected time if applicable
		if room.HostDisconnectedAt != nil {
			newTime := room.HostDisconnectedAt.Add(shutdownDuration)
			room.HostDisconnectedAt = &newTime
		}

		// Find and set the host reference
		hostSession, exists := room.DisconnectedUsers[persistentRoom.HostID]
		if exists {
			// Create a placeholder client for the host
			room.Host = &Client{
				ID:           hostSession.UserID,
				Username:     hostSession.Username,
				SessionToken: "", // Will be set on reconnection
			}
		}

		s.rooms[room.Code] = room
		s.logger.Info("Restored room",
			zap.String("code", room.Code),
			zap.String("host_id", persistentRoom.HostID),
			zap.Int("disconnected_users", len(room.DisconnectedUsers)))
	}

	// Restore sessions
	for _, ps := range state.Sessions {
		// Adjust disconnect time to account for shutdown duration
		session := &Session{
			UserID:       ps.UserID,
			Username:     ps.Username,
			RoomCode:     ps.RoomCode,
			IsHost:       ps.IsHost,
			DisconnectAt: ps.DisconnectAt.Add(shutdownDuration),
		}
		s.sessions[ps.Token] = session
	}

	s.logger.Info("State restoration complete",
		zap.Int("rooms_restored", len(state.Rooms)),
		zap.Int("sessions_restored", len(state.Sessions)))

	// Delete the state file after successful load
	if err := os.Remove(StateFile); err != nil {
		s.logger.Warn("Failed to remove state file", zap.Error(err))
	}

	return nil
}
