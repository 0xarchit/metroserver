package server

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"go.etcd.io/bbolt"
	"go.uber.org/zap"
)

const (
	StateFile        = "server_state.json" // legacy migration from pre-bbolt deployments
	MaxStateFileSize = 50 * 1024 * 1024
)

var (
	serverStateBucket = []byte("server_state")
	serverStateKey    = []byte("latest")
)

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
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	state := PersistentState{
		ServerShutdownTime: now,
		Rooms:              make([]PersistentRoom, 0),
		Sessions:           make([]PersistentSession, 0),
	}
	sessions := make(map[string]PersistentSession)
	for token, session := range s.sessions {
		if token == "" || session == nil {
			continue
		}
		sessions[token] = PersistentSession{
			Token:        token,
			UserID:       session.UserID,
			Username:     session.Username,
			RoomCode:     session.RoomCode,
			IsHost:       session.IsHost,
			DisconnectAt: session.DisconnectAt,
		}
	}

	// Save all rooms
	for _, room := range s.rooms {
		room.mu.RLock()
		stateCopy := cloneRoomState(room.State)
		if stateCopy == nil {
			room.mu.RUnlock()
			continue
		}
		for i := range stateCopy.Users {
			stateCopy.Users[i].IsConnected = false
		}

		disconnectedUsers := make(map[string]*Session, len(room.DisconnectedUsers)+len(room.Clients))
		for userID, session := range room.DisconnectedUsers {
			if session == nil {
				continue
			}
			copySession := *session
			disconnectedUsers[userID] = &copySession
		}

		for userID, client := range room.Clients {
			if client == nil {
				continue
			}
			token := client.session()
			if token == "" {
				token = s.generateSessionToken()
				client.setSessionToken(token)
			}
			isHost := room.State.HostID == userID
			session := &Session{
				UserID:       userID,
				Username:     client.userName(),
				RoomCode:     room.Code,
				IsHost:       isHost,
				DisconnectAt: now,
			}
			disconnectedUsers[userID] = session
			sessions[token] = PersistentSession{
				Token:        token,
				UserID:       session.UserID,
				Username:     session.Username,
				RoomCode:     session.RoomCode,
				IsHost:       session.IsHost,
				DisconnectAt: session.DisconnectAt,
			}
		}

		// Convert pending suggestions
		pendingSuggestions := make([]PersistentSuggestion, 0, len(room.PendingSuggestions))
		for _, suggestion := range room.PendingSuggestions {
			if suggestion == nil {
				continue
			}
			pendingSuggestions = append(pendingSuggestions, PersistentSuggestion{
				ID:           suggestion.ID,
				FromUserID:   suggestion.FromUserID,
				FromUsername: suggestion.FromUsername,
				Track:        cloneTrackInfo(suggestion.Track),
			})
		}

		// Get host ID from room state (room.Host can be nil after disconnection)
		hostID := room.State.HostID

		hostDisconnectedAt := room.HostDisconnectedAt
		if room.State.HostID != "" && room.Clients[room.State.HostID] != nil {
			shutdownTime := now
			hostDisconnectedAt = &shutdownTime
		}

		persistentRoom := PersistentRoom{
			Code:               room.Code,
			HostID:             hostID,
			State:              stateCopy,
			DisconnectedUsers:  disconnectedUsers,
			PendingSuggestions: pendingSuggestions,
			HostDisconnectedAt: hostDisconnectedAt,
		}

		state.Rooms = append(state.Rooms, persistentRoom)
		room.mu.RUnlock()
	}

	tokens := make([]string, 0, len(sessions))
	for token := range sessions {
		tokens = append(tokens, token)
	}
	sort.Strings(tokens)
	for _, token := range tokens {
		state.Sessions = append(state.Sessions, sessions[token])
	}

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	if len(data) > MaxStateFileSize {
		return fmt.Errorf("server state exceeds %d bytes", MaxStateFileSize)
	}
	if err := s.database.saveServerState(data); err != nil {
		return fmt.Errorf("write server state: %w", err)
	}

	s.logger.Info("Server state saved",
		zap.Int("rooms", len(state.Rooms)),
		zap.Int("sessions", len(state.Sessions)))

	return nil
}

func (d *database) saveServerState(data []byte) error {
	if d == nil || d.db == nil {
		return fmt.Errorf("database is not configured")
	}
	return d.db.Update(func(tx *bbolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(serverStateBucket)
		if err != nil {
			return err
		}
		return bucket.Put(serverStateKey, data)
	})
}

func (d *database) loadServerState() ([]byte, error) {
	if d == nil || d.db == nil {
		return nil, fmt.Errorf("database is not configured")
	}
	var data []byte
	err := d.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(serverStateBucket)
		if bucket != nil {
			data = append(data, bucket.Get(serverStateKey)...)
		}
		return nil
	})
	return data, err
}

func (d *database) consumeServerState() error {
	if d == nil || d.db == nil {
		return fmt.Errorf("database is not configured")
	}
	return d.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(serverStateBucket)
		if bucket == nil {
			return nil
		}
		return bucket.Delete(serverStateKey)
	})
}

// LoadState restores the most recent shutdown snapshot. The JSON fallback lets
// the first bbolt-enabled deployment consume state written by the old binary.
func (s *Server) LoadState() error {
	data, err := s.database.loadServerState()
	if err != nil {
		return fmt.Errorf("read server state: %w", err)
	}
	fromDatabase := len(data) != 0
	if !fromDatabase {
		info, statErr := os.Stat(StateFile)
		if os.IsNotExist(statErr) {
			s.logger.Info("No previous server state found, starting fresh")
			return nil
		}
		if statErr != nil {
			return fmt.Errorf("stat legacy state file: %w", statErr)
		}
		if info.Size() > MaxStateFileSize {
			return fmt.Errorf("legacy state file exceeds %d bytes", MaxStateFileSize)
		}
		data, err = os.ReadFile(StateFile)
		if err != nil {
			return fmt.Errorf("read legacy state file: %w", err)
		}
	}
	if len(data) > MaxStateFileSize {
		return fmt.Errorf("server state exceeds %d bytes", MaxStateFileSize)
	}

	var state PersistentState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("unmarshal state: %w", err)
	}
	if fromDatabase {
		if err := os.Remove(StateFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale legacy state file: %w", err)
		}
		if err := s.database.consumeServerState(); err != nil {
			return fmt.Errorf("consume server state: %w", err)
		}
	} else if err := os.Remove(StateFile); err != nil {
		return fmt.Errorf("consume legacy state file: %w", err)
	}

	// Calculate time elapsed since shutdown
	shutdownDuration := time.Since(state.ServerShutdownTime)
	s.logger.Info("Loading previous state",
		zap.Duration("offline_duration", shutdownDuration),
		zap.Int("rooms", len(state.Rooms)),
		zap.Int("sessions", len(state.Sessions)))

	s.mu.Lock()
	defer s.mu.Unlock()

	// Restore rooms
	for _, persistentRoom := range state.Rooms {
		if persistentRoom.State == nil {
			s.logger.Warn("Skipping restored room without state", zap.String("code", persistentRoom.Code))
			continue
		}
		disconnectedUsers := persistentRoom.DisconnectedUsers
		if disconnectedUsers == nil {
			disconnectedUsers = make(map[string]*Session)
		}
		if persistentRoom.State != nil {
			currentTrackID := ""
			if persistentRoom.State.CurrentTrack != nil {
				currentTrackID = persistentRoom.State.CurrentTrack.ID
			}
			persistentRoom.State.Queue = sanitizeUpcomingQueue(persistentRoom.State.Queue, currentTrackID)
			for i := range persistentRoom.State.Users {
				persistentRoom.State.Users[i].IsConnected = false
			}
		}

		room := &Room{
			Code:               persistentRoom.Code,
			Host:               nil, // The host pointer is only set when the real client reconnects.
			Clients:            make(map[string]*Client),
			PendingJoins:       make(map[string]*Client),
			PendingSuggestions: make(map[string]*Suggestion),
			DisconnectedUsers:  disconnectedUsers,
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
		for userID, session := range room.DisconnectedUsers {
			if session == nil {
				delete(room.DisconnectedUsers, userID)
				continue
			}
			session.DisconnectAt = session.DisconnectAt.Add(shutdownDuration)
		}

		// Update host disconnected time if applicable
		if room.HostDisconnectedAt != nil {
			newTime := room.HostDisconnectedAt.Add(shutdownDuration)
			room.HostDisconnectedAt = &newTime
		}

		if room.HostDisconnectedAt == nil && persistentRoom.HostID != "" {
			if hostSession, exists := room.DisconnectedUsers[persistentRoom.HostID]; exists && hostSession != nil {
				hostDisconnectedAt := hostSession.DisconnectAt
				room.HostDisconnectedAt = &hostDisconnectedAt
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

	return nil
}
