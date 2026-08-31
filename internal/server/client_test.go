package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pb "github.com/MetrolistGroup/metroserver/proto"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

func TestWebSocketHeartbeatHasPongBudget(t *testing.T) {
	if PingInterval >= ReadTimeout {
		t.Fatalf("ping interval %s must be shorter than read timeout %s", PingInterval, ReadTimeout)
	}
}

func TestWebSocketPumpsRoundTripAndRemoveClient(t *testing.T) {
	server := testServer()
	httpServer := httptest.NewServer(http.HandlerFunc(server.handleWebSocket))
	t.Cleanup(httpServer.Close)

	url := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}

	codec := NewMessageCodec(false)
	message, err := codec.Encode(MsgTypePing, PingPayload{ClientTime: 123, Sequence: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, message); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	messageType, response, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("message type = %d, want binary", messageType)
	}
	responseType, payload, err := codec.Decode(response)
	if err != nil {
		t.Fatal(err)
	}
	if responseType != MsgTypePong {
		t.Fatalf("response type = %q, want %q", responseType, MsgTypePong)
	}
	var pong pb.PongPayload
	if err := proto.Unmarshal(payload, &pong); err != nil {
		t.Fatal(err)
	}
	if pong.ClientTime != 123 || pong.Sequence != 7 || pong.ServerReceiveTime == 0 || pong.ServerSendTime < pong.ServerReceiveTime {
		t.Fatalf("unexpected pong: %#v", &pong)
	}

	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		server.mu.RLock()
		clientCount := len(server.clients)
		server.mu.RUnlock()
		if clientCount == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server retained %d closed WebSocket clients", clientCount)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestClientAllowMessageRateLimit(t *testing.T) {
	client := newClient("client", nil)
	start := time.Unix(100, 0)

	for i := 0; i < MaxMessagesPerWindow; i++ {
		if !client.allowMessage(start) {
			t.Fatalf("message %d was rejected before the limit", i+1)
		}
	}
	if client.allowMessage(start.Add(MessageRateWindow - time.Nanosecond)) {
		t.Fatal("message above the limit was accepted in the same window")
	}
	if !client.allowMessage(start.Add(MessageRateWindow)) {
		t.Fatal("first message in a new window was rejected")
	}
}

func TestClientLimitsSyncResponses(t *testing.T) {
	client := newClient("client", nil)
	now := time.Now()
	if !client.allowSyncResponse(now) {
		t.Fatal("first sync response was rejected")
	}
	if client.allowSyncResponse(now.Add(SyncResponseInterval - time.Nanosecond)) {
		t.Fatal("sync response inside the cooldown was accepted")
	}
	if !client.allowSyncResponse(now.Add(SyncResponseInterval)) {
		t.Fatal("sync response after the cooldown was rejected")
	}
	for len(client.Send) < cap(client.Send) {
		client.Send <- nil
	}
	if client.allowSyncResponse(now.Add(2 * SyncResponseInterval)) {
		t.Fatal("sync response was accepted with a full send queue")
	}
}

func TestClientCloseSendIsIdempotent(t *testing.T) {
	client := newClient("client", nil)
	client.closeSend()
	client.closeSend()

	if !client.isClosed() {
		t.Fatal("client was not marked closed")
	}
	if _, open := <-client.Send; open {
		t.Fatal("send channel was not closed")
	}
}

func TestClientClosesWhenSendBufferIsFull(t *testing.T) {
	client := newClient("client", nil)
	for i := 0; i < cap(client.Send); i++ {
		client.Send <- nil
	}

	client.sendMessage(zap.NewNop(), MsgTypePong, nil)
	if !client.isClosed() {
		t.Fatal("client with a full send buffer was not closed")
	}
}

func TestBroadcastRespectsClientCompression(t *testing.T) {
	compressedClient := newClient("compressed", nil)
	uncompressedClient := newClient("uncompressed", nil)
	uncompressedClient.codec.setCompressionEnabled(false)

	sendMessageToClients(zap.NewNop(), []*Client{compressedClient, uncompressedClient}, MsgTypeJoinRejected, JoinRejectedPayload{
		Reason: strings.Repeat("compressible", 100),
	})

	for _, test := range []struct {
		name           string
		client         *Client
		wantCompressed bool
	}{
		{name: "compressed", client: compressedClient, wantCompressed: true},
		{name: "uncompressed", client: uncompressedClient},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := <-test.client.Send
			var envelope pb.Envelope
			if err := proto.Unmarshal(message, &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Compressed != test.wantCompressed {
				t.Fatalf("compressed = %v, want %v", envelope.Compressed, test.wantCompressed)
			}
		})
	}
}
