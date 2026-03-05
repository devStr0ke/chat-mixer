// test_chat.go — standalone integration test for WebSocket chat
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

const base = "http://localhost:8080"

type authResp struct {
	Token string `json:"token"`
	User  struct {
		ID string `json:"id"`
	} `json:"user"`
}

type matchResp struct {
	RoomID  string `json:"room_id,omitempty"`
	Message string `json:"message,omitempty"`
}

type msg struct {
	ID       string `json:"id"`
	SenderID string `json:"sender_id"`
	Content  string `json:"content"`
	SentAt   string `json:"sent_at"`
}

func register(pseudo, email, country string) authResp {
	body, _ := json.Marshal(map[string]string{
		"pseudo": pseudo, "email": email, "country": country, "password": "secret123",
	})
	resp, err := http.Post(base+"/auth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	var ar authResp
	json.NewDecoder(resp.Body).Decode(&ar)
	return ar
}

func joinPool(token string) matchResp {
	req, _ := http.NewRequest("POST", base+"/pool/join", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	var mr matchResp
	json.NewDecoder(resp.Body).Decode(&mr)
	return mr
}

func connectWS(roomID, token string) *websocket.Conn {
	u := url.URL{Scheme: "ws", Host: "localhost:8080", Path: "/ws/" + roomID, RawQuery: "token=" + token}
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatalf("ws dial failed: %v", err)
	}
	return conn
}

func getMessages(roomID, token string) []msg {
	req, _ := http.NewRequest("GET", base+"/rooms/"+roomID+"/messages", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var msgs []msg
	json.Unmarshal(b, &msgs)
	return msgs
}

func main() {
	ts := fmt.Sprintf("%d", time.Now().UnixNano())

	fmt.Println("=== Register two users ===")
	userA := register("testa_"+ts, "testa_"+ts+"@test.com", "FR")
	userB := register("testb_"+ts, "testb_"+ts+"@test.com", "NG")
	fmt.Printf("  User A: %s\n  User B: %s\n", userA.User.ID[:8], userB.User.ID[:8])

	fmt.Println("\n=== Match them ===")
	joinPool(userA.Token) // waits
	mr := joinPool(userB.Token)
	fmt.Printf("  Room: %s\n", mr.RoomID)

	fmt.Println("\n=== Connect both via WebSocket ===")
	wsA := connectWS(mr.RoomID, userA.Token)
	wsB := connectWS(mr.RoomID, userB.Token)
	defer wsA.Close()
	defer wsB.Close()

	// B listens in background
	received := make(chan string, 10)
	go func() {
		for {
			_, m, err := wsB.ReadMessage()
			if err != nil {
				return
			}
			received <- string(m)
		}
	}()

	fmt.Println("\n=== User A sends messages ===")
	wsA.WriteMessage(websocket.TextMessage, []byte("Hello from A!"))
	wsA.WriteMessage(websocket.TextMessage, []byte("How are you?"))
	time.Sleep(500 * time.Millisecond)

	fmt.Println("\n=== User B should have received them ===")
	for i := 0; i < 2; i++ {
		select {
		case m := <-received:
			fmt.Printf("  B received: %q\n", m)
		case <-time.After(2 * time.Second):
			fmt.Println("  ⚠️  timeout waiting for message")
		}
	}

	// A listens in background
	receivedA := make(chan string, 10)
	go func() {
		for {
			_, m, err := wsA.ReadMessage()
			if err != nil {
				return
			}
			receivedA <- string(m)
		}
	}()

	fmt.Println("\n=== User B sends a message ===")
	wsB.WriteMessage(websocket.TextMessage, []byte("Hey A, all good!"))
	time.Sleep(500 * time.Millisecond)

	select {
	case m := <-receivedA:
		fmt.Printf("  A received: %q\n", m)
	case <-time.After(2 * time.Second):
		fmt.Println("  ⚠️  timeout waiting for message")
	}

	fmt.Println("\n=== Verify messages in DB ===")
	msgs := getMessages(mr.RoomID, userA.Token)
	for _, m := range msgs {
		fmt.Printf("  [%s] %s: %s\n", m.SentAt[:19], m.SenderID[:8], m.Content)
	}
	fmt.Printf("  Total: %d messages\n", len(msgs))

	fmt.Println("\nDONE ✅")
}
