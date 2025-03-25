package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"translation-service/translation"
	"translation/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins in production
	},
}

type Client struct {
	ID       string
	Conn     *websocket.Conn
	Language string
	IsSpeaker bool
}

type Message struct {
	Text     string `json:"text"`
	Language string `json:"language"`
}

var clients = make(map[*Client]bool)
var broadcast = make(chan []byte)
var translator *translation.Translator
var hub *websocket.Hub

func init() {
	// Load environment variables first
	if err := godotenv.Load(); err != nil {
		log.Println("Warning: .env file not found")
	}

	// Initialize translator
	var err error
	translator, err = translation.NewTranslator()
	if err != nil {
		log.Fatalf("Failed to initialize translator: %v", err)
	}

	// Initialize WebSocket hub
	hub = websocket.NewHub()
	go hub.Run()
}

func handleWebSocket(c *gin.Context) {
	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println(err)
		return
	}

	client := &Client{
		ID:       c.Query("id"),
		Conn:     ws,
		Language: c.Query("lang"),
		IsSpeaker: c.Query("role") == "speaker",
	}
	clients[client] = true
	log.Printf("New client connected - ID: %s, Role: %s, Language: %s", client.ID, c.Query("role"), client.Language)

	defer func() {
		delete(clients, client)
		client.Conn.Close()
		log.Printf("Client disconnected - ID: %s", client.ID)
	}()

	for {
		messageType, message, err := ws.ReadMessage()
		if err != nil {
			log.Printf("Error reading message: %v", err)
			break
		}

		if messageType == websocket.TextMessage {
			log.Printf("Received message: %s", string(message))
			
			// Parse the incoming message
			var msg Message
			if err := json.Unmarshal(message, &msg); err != nil {
				log.Printf("Error parsing message: %v", err)
				continue
			}

			// If this is from a speaker, translate for all audience members
			if client.IsSpeaker {
				for audienceClient := range clients {
					if !audienceClient.IsSpeaker && audienceClient.Language != msg.Language {
						translatedText, err := translator.Translate(msg.Text, msg.Language, audienceClient.Language)
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

						err = audienceClient.Conn.WriteMessage(websocket.TextMessage, responseJSON)
						if err != nil {
							log.Printf("Error sending translated message: %v", err)
							continue
						}
						log.Printf("Translated message sent to client %s: %s", audienceClient.ID, string(responseJSON))
					}
				}
			}
		}
	}
}

func handleMessages() {
	for {
		msg := <-broadcast
		for client := range clients {
			err := client.Conn.WriteMessage(websocket.TextMessage, msg)
			if err != nil {
				log.Printf("Error writing message: %v", err)
				client.Conn.Close()
				delete(clients, client)
				break
			}
			log.Printf("Message sent to client %s: %s", client.ID, string(msg))
		}
	}
}

func main() {
	go handleMessages()

	router := gin.Default()

	// Serve static files from the templates directory
	router.LoadHTMLGlob("templates/*")
	router.Static("/static", "./static")

	// Routes
	router.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", nil)
	})

	router.GET("/speaker", func(c *gin.Context) {
		c.HTML(http.StatusOK, "speaker.html", nil)
	})

	router.GET("/audience", func(c *gin.Context) {
		c.HTML(http.StatusOK, "audience.html", nil)
	})

	router.GET("/ws", func(c *gin.Context) {
		websocket.ServeWs(hub, translator, c.Writer, c.Request)
	})

	// Get port from environment variable or use default
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Start server
	log.Printf("Server starting on port %s", port)
	if err := router.Run(":" + port); err != nil {
		log.Fatal("Failed to start server:", err)
	}
} 