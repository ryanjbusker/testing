package websocket

import (
	"encoding/json"
	"log"
	"net/http"

	"translation/translation"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins in production
	},
}

type Client struct {
	ID        string
	Conn      *websocket.Conn
	Language  string
	IsSpeaker bool
	Hub       *Hub
}

type Message struct {
	Text     string `json:"text"`
	Language string `json:"language"`
}

type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	translator *translation.Translator
}

func NewHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				client.Conn.Close()
				log.Printf("Client disconnected - ID: %s", client.ID)
			}

		case message := <-h.broadcast:
			for client := range h.clients {
				err := client.Conn.WriteMessage(websocket.TextMessage, message)
				if err != nil {
					log.Printf("Error writing message: %v", err)
					client.Conn.Close()
					delete(h.clients, client)
					break
				}
			}
		}
	}
}

func ServeWs(hub *Hub, translator *translation.Translator, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	client := &Client{
		ID:        r.URL.Query().Get("id"),
		Conn:      conn,
		Language:  r.URL.Query().Get("lang"),
		IsSpeaker: r.URL.Query().Get("role") == "speaker",
		Hub:       hub,
	}

	hub.register <- client
	hub.translator = translator

	go client.writePump()
	go client.readPump()
}

func (c *Client) readPump() {
	defer func() {
		c.Hub.unregister <- c
		c.Conn.Close()
	}()

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("error: %v", err)
			}
			break
		}

		if c.IsSpeaker {
			var msg Message
			if err := json.Unmarshal(message, &msg); err != nil {
				log.Printf("Error parsing message: %v", err)
				continue
			}

			// Translate for all audience members
			for audienceClient := range c.Hub.clients {
				if !audienceClient.IsSpeaker && audienceClient.Language != msg.Language {
					translatedText, err := c.Hub.translator.Translate(msg.Text, msg.Language, audienceClient.Language)
					if err != nil {
						log.Printf("Translation error: %v", err)
						continue
					}

					response := Message{
						Text:     translatedText,
						Language: audienceClient.Language,
					}

					responseJSON, err := json.Marshal(response)
					if err != nil {
						log.Printf("Error marshaling response: %v", err)
						continue
					}

					audienceClient.Conn.WriteMessage(websocket.TextMessage, responseJSON)
					log.Printf("Translated message sent to client %s: %s", audienceClient.ID, string(responseJSON))
				}
			}
		}
	}
}

func (c *Client) writePump() {
	defer func() {
		c.Conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.Hub.broadcast:
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.Conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			n := len(c.Hub.broadcast)
			for i := 0; i < n; i++ {
				w.Write([]byte{'\n'})
				w.Write(<-c.Hub.broadcast)
			}

			if err := w.Close(); err != nil {
				return
			}
		}
	}
}
