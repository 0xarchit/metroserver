package server

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

type Client struct {
	id                 atomic.Value // string
	username           atomic.Value // string
	sessionToken       atomic.Value // string
	room               atomic.Pointer[Room]
	pendingRoom        atomic.Pointer[Room]
	capabilitiesSet    bool
	affiliationStarted bool
	Conn               *websocket.Conn
	Send               chan []byte
	closed             bool
	rateWindowStart    time.Time
	rateMessageCount   int
	lastSyncResponse   time.Time
	mu                 sync.Mutex
	negotiationMu      sync.Mutex
	codec              *MessageCodec // Message codec for encoding/decoding
}

func newClient(id string, conn *websocket.Conn) *Client {
	c := &Client{
		Conn:  conn,
		Send:  make(chan []byte, 256),
		codec: NewMessageCodec(true),
	}
	c.setClientID(id)
	return c
}

func loadAtomicString(v *atomic.Value) string {
	raw := v.Load()
	if raw == nil {
		return ""
	}
	return raw.(string)
}

func (c *Client) clientID() string {
	return loadAtomicString(&c.id)
}

func (c *Client) setClientID(id string) {
	c.id.Store(id)
}

func (c *Client) userName() string {
	return loadAtomicString(&c.username)
}

func (c *Client) setUsername(username string) {
	c.username.Store(username)
}

func (c *Client) session() string {
	return loadAtomicString(&c.sessionToken)
}

func (c *Client) setSessionToken(token string) {
	c.sessionToken.Store(token)
}

func (c *Client) currentRoom() *Room {
	return c.room.Load()
}

func (c *Client) setRoom(room *Room) {
	if room != nil {
		c.markAffiliationStarted()
	}
	c.room.Store(room)
}

func (c *Client) trySetRoom(room *Room) bool {
	if room == nil {
		return false
	}
	c.markAffiliationStarted()
	return c.room.CompareAndSwap(nil, room)
}

func (c *Client) clearRoom(room *Room) bool {
	return room != nil && c.room.CompareAndSwap(room, nil)
}

func (c *Client) currentPendingRoom() *Room {
	return c.pendingRoom.Load()
}

func (c *Client) trySetPendingRoom(room *Room) bool {
	if room == nil || c.currentRoom() != nil {
		return false
	}
	c.markAffiliationStarted()
	if !c.pendingRoom.CompareAndSwap(nil, room) {
		return false
	}
	if c.currentRoom() != nil {
		c.pendingRoom.CompareAndSwap(room, nil)
		return false
	}
	return true
}

func (c *Client) clearPendingRoom(room *Room) bool {
	return room != nil && c.pendingRoom.CompareAndSwap(room, nil)
}

func (c *Client) markAffiliationStarted() {
	c.negotiationMu.Lock()
	c.affiliationStarted = true
	c.negotiationMu.Unlock()
}

func (c *Client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *Client) closeSend() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		if c.Send != nil {
			close(c.Send)
		}
	}
	c.mu.Unlock()
}

func (c *Client) allowMessage(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.rateWindowStart.IsZero() || now.Sub(c.rateWindowStart) >= MessageRateWindow {
		c.rateWindowStart = now
		c.rateMessageCount = 0
	}

	if c.rateMessageCount >= MaxMessagesPerWindow {
		return false
	}

	c.rateMessageCount++
	return true
}

func (c *Client) allowSyncResponse(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.Send == nil || len(c.Send) >= cap(c.Send) {
		return false
	}
	if !c.lastSyncResponse.IsZero() && now.Sub(c.lastSyncResponse) < SyncResponseInterval {
		return false
	}
	c.lastSyncResponse = now
	return true
}

func (c *Client) writePump(logger *zap.Logger) {
	ticker := time.NewTicker(PingInterval)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.Send:
			if err := c.Conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
				logger.Debug("Failed to set write deadline", zap.String("client_id", c.clientID()), zap.Error(err))
				return
			}
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := c.Conn.WriteMessage(websocket.BinaryMessage, message); err != nil {
				logger.Debug("Write error for client", zap.String("client_id", c.clientID()), zap.Error(err))
				return
			}

		case <-ticker.C:
			if err := c.Conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
				logger.Debug("Failed to set write deadline", zap.String("client_id", c.clientID()), zap.Error(err))
				return
			}
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) readPump(s *Server) {
	defer func() {
		s.removeClient(c)
		c.Conn.Close()
	}()

	c.Conn.SetReadLimit(MaxReadMessageSize)
	if err := c.Conn.SetReadDeadline(time.Now().Add(ReadTimeout)); err != nil {
		s.logger.Debug("Failed to set read deadline", zap.String("client_id", c.clientID()), zap.Error(err))
	}
	c.Conn.SetPongHandler(func(string) error {
		if err := c.Conn.SetReadDeadline(time.Now().Add(ReadTimeout)); err != nil {
			s.logger.Debug("Failed to set read deadline in pong handler", zap.String("client_id", c.clientID()), zap.Error(err))
		}
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				s.logger.Debug("Read error for client", zap.String("client_id", c.clientID()), zap.Error(err))
			}
			break
		}

		if !c.allowMessage(time.Now()) {
			c.sendError(s.logger, "rate_limited", "Too many messages")
			continue
		}

		if err := c.Conn.SetReadDeadline(time.Now().Add(ReadTimeout)); err != nil {
			s.logger.Debug("Failed to refresh read deadline", zap.String("client_id", c.clientID()), zap.Error(err))
			break
		}
		s.handleMessage(c, message)
	}
}

func (c *Client) sendMessage(logger *zap.Logger, msgType string, payload interface{}) {
	if c == nil || c.codec == nil || c.Send == nil {
		return
	}

	// Use the client's codec to encode the message
	msgData, err := c.codec.Encode(msgType, payload)
	if err != nil {
		logger.Error("Error encoding message", zap.String("message_type", msgType), zap.String("payload_type", fmt.Sprintf("%T", payload)), zap.Error(err))
		return
	}

	logger.Debug("Message encoded successfully",
		zap.String("message_type", msgType),
		zap.String("payload_type", fmt.Sprintf("%T", payload)),
		zap.Int("encoded_size_bytes", len(msgData)))

	c.sendEncodedMessage(logger, msgType, msgData)
}

func (c *Client) sendEncodedMessage(logger *zap.Logger, msgType string, msgData []byte) {
	if c == nil || c.Send == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		logger.Debug("Attempted to send to closed client", zap.String("client_id", c.clientID()))
		return
	}

	select {
	case c.Send <- msgData:
		logger.Debug("Message queued for sending", zap.String("message_type", msgType), zap.Int("size", len(msgData)))
	default:
		logger.Warn("Client send buffer full; closing slow client", zap.String("client_id", c.clientID()))
		c.closed = true
		close(c.Send)
	}
}

func sendMessageToClients(logger *zap.Logger, clients []*Client, msgType string, payload interface{}) {
	if len(clients) == 0 {
		return
	}

	var encoded [2][]byte
	var encodedReady [2]bool
	for _, client := range clients {
		if client == nil {
			continue
		}
		compressionIndex := 0
		if client.codec != nil && client.codec.compressionEnabled.Load() {
			compressionIndex = 1
		}
		if !encodedReady[compressionIndex] {
			msgData, err := NewMessageCodec(compressionIndex == 1).Encode(msgType, payload)
			if err != nil {
				logger.Error("Error encoding broadcast", zap.String("message_type", msgType), zap.Error(err))
				return
			}
			encoded[compressionIndex] = msgData
			encodedReady[compressionIndex] = true
		}
		client.sendEncodedMessage(logger, msgType, encoded[compressionIndex])
	}
}

func (c *Client) sendError(logger *zap.Logger, code, message string) {
	c.sendMessage(logger, MsgTypeError, ErrorPayload{
		Code:    code,
		Message: message,
	})
}
