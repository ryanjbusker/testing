package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"translation/translation"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/joho/godotenv"
)

var translator *translation.Translator
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins in development
	},
}

type Stream struct {
	Speakers    map[string]*Speaker
	Audience    map[string]*Audience
	CreatedAt   time.Time
	Name        string
	Description string
	IsActive    bool
}

type Speaker struct {
	Conn       *websocket.Conn
	Language   string
	LastActive time.Time
}

type Audience struct {
	Conn       *websocket.Conn
	Language   string
	LastActive time.Time
}

var (
	stream = &Stream{
		Name:        "Live Translation",
		Description: "Real-time translation stream",
		Speakers:    make(map[string]*Speaker),
		Audience:    make(map[string]*Audience),
		IsActive:    true,
		CreatedAt:   time.Now(),
	}
	mu sync.RWMutex
)

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
}

func main() {
	router := gin.Default()

	// Log the current working directory and template directory
	wd, _ := os.Getwd()
	log.Printf("Current working directory: %s", wd)
	templatesDir := filepath.Join(wd, "templates")
	log.Printf("Templates directory: %s", templatesDir)

	// List all files in the templates directory
	files, err := os.ReadDir(templatesDir)
	if err != nil {
		log.Printf("Error reading templates directory: %v", err)
	} else {
		log.Printf("Files in templates directory:")
		for _, file := range files {
			log.Printf("- %s", file.Name())
			// Read and log the first line of each template file
			if filepath.Ext(file.Name()) == ".html" {
				content, err := os.ReadFile(filepath.Join(templatesDir, file.Name()))
				if err != nil {
					log.Printf("Error reading %s: %v", file.Name(), err)
				} else {
					log.Printf("First line of %s: %s", file.Name(), string(content[:100]))
				}
			}
		}
	}

	// Serve static files from the static directory
	router.Static("/static", "./static")

	// Load all HTML templates from the templates directory
	router.LoadHTMLGlob("templates/*.html")
	log.Printf("Loaded templates from %s", templatesDir)

	// Routes
	router.GET("/", func(c *gin.Context) {
		log.Printf("Serving index.html")
		c.HTML(http.StatusOK, "index.html", gin.H{
			"title": "Translation Service",
		})
	})

	router.GET("/speaker", func(c *gin.Context) {
		log.Printf("Serving speaker.html")
		c.HTML(http.StatusOK, "speaker.html", gin.H{
			"title": "Speaker Page",
		})
	})

	router.GET("/speaker/", func(c *gin.Context) {
		log.Printf("Serving speaker.html (with trailing slash)")
		c.HTML(http.StatusOK, "speaker.html", gin.H{
			"title": "Speaker Page",
		})
	})

	router.GET("/audience", func(c *gin.Context) {
		log.Printf("Serving audience.html")
		c.HTML(http.StatusOK, "audience.html", gin.H{
			"title": "Audience Page",
		})
	})

	router.GET("/audience/", func(c *gin.Context) {
		log.Printf("Serving audience.html (with trailing slash)")
		c.HTML(http.StatusOK, "audience.html", gin.H{
			"title": "Audience Page",
		})
	})

	router.GET("/contact", func(c *gin.Context) {
		log.Printf("Serving contact.html")
		c.HTML(http.StatusOK, "contact.html", gin.H{
			"title": "Contact Us",
		})
	})

	router.GET("/streams", func(c *gin.Context) {
		mu.RLock()
		activeStreams := make([]map[string]interface{}, 0)
		activeStreams = append(activeStreams, map[string]interface{}{
			"name":        stream.Name,
			"description": stream.Description,
			"speakers":    len(stream.Speakers),
			"audience":    len(stream.Audience),
			"created_at":  stream.CreatedAt,
		})
		mu.RUnlock()

		log.Printf("Found %d active streams", len(activeStreams))
		for _, stream := range activeStreams {
			log.Printf("Stream: %s, Name: %s, Speakers: %d, Audience: %d",
				stream["name"], stream["name"], stream["speakers"], stream["audience"])
		}

		c.HTML(http.StatusOK, "streams.html", gin.H{
			"title":   "Active Streams",
			"streams": activeStreams,
		})
	})

	router.GET("/ws", func(c *gin.Context) {
		handleWebSocket(c)
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

func generatePassword() string {
	return fmt.Sprintf("%05d", rand.Intn(100000))
}

func handleWebSocket(c *gin.Context) {
	role := c.Query("role")
	lang := c.Query("lang")

	log.Printf("WebSocket connection request - Role: %s", role)

	// Upgrade the HTTP connection to a WebSocket connection
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to establish WebSocket connection"})
		return
	}

	// Send initial connection success message
	successMsg := map[string]interface{}{
		"type":       "connected",
		"role":       role,
		"streamName": stream.Name,
		"streamDesc": stream.Description,
	}
	successJSON, _ := json.Marshal(successMsg)
	if err := conn.WriteMessage(websocket.TextMessage, successJSON); err != nil {
		log.Printf("Failed to send connection success message: %v", err)
		conn.Close()
		return
	}

	if role == "speaker" {
		speakerID := conn.RemoteAddr().String()
		speaker := &Speaker{
			Conn:       conn,
			Language:   lang,
			LastActive: time.Now(),
		}
		stream.Speakers[speakerID] = speaker
		log.Printf("Added speaker %s", speakerID)
		defer func() {
			conn.Close()
			delete(stream.Speakers, speakerID)
			log.Printf("Speaker %s left", speakerID)
			if len(stream.Speakers) == 0 && len(stream.Audience) == 0 {
				stream.IsActive = false
				log.Printf("Stream is now inactive")
			}
			// Notify others that this speaker left
			if len(stream.Audience) > 0 {
				leaveMsg := map[string]interface{}{
					"type":    "speaker_left",
					"speaker": speakerID,
				}
				leaveJSON, _ := json.Marshal(leaveMsg)
				for _, audience := range stream.Audience {
					audience.Conn.WriteMessage(websocket.TextMessage, leaveJSON)
				}
			}
		}()
	} else if role == "audience" {
		audienceID := conn.RemoteAddr().String()
		audience := &Audience{
			Conn:       conn,
			Language:   lang,
			LastActive: time.Now(),
		}
		stream.Audience[audienceID] = audience
		log.Printf("Added audience member %s", audienceID)
		defer func() {
			conn.Close()
			delete(stream.Audience, audienceID)
			log.Printf("Audience member %s left", audienceID)
			if len(stream.Speakers) == 0 && len(stream.Audience) == 0 {
				stream.IsActive = false
				log.Printf("Stream is now inactive")
			}
		}()
	}

	// Handle incoming messages
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Error reading message: %v", err)
			break
		}

		if messageType == websocket.TextMessage {
			var data map[string]interface{}
			if err := json.Unmarshal(message, &data); err != nil {
				log.Printf("Error parsing message: %v", err)
				continue
			}

			if data["type"] == "speech" {
				text := data["text"].(string)
				speakerID := conn.RemoteAddr().String()
				speaker := stream.Speakers[speakerID]
				if speaker != nil {
					// Translate for each audience member
					for _, audience := range stream.Audience {
						translatedText, err := translateText(text, speaker.Language, audience.Language)
						if err != nil {
							log.Printf("Translation error: %v", err)
							continue
						}
						response := map[string]interface{}{
							"type":     "translation",
							"text":     translatedText,
							"speaker":  speakerID,
							"original": text,
						}
						responseJSON, _ := json.Marshal(response)
						audience.Conn.WriteMessage(websocket.TextMessage, responseJSON)
					}
				}
			}
		}
	}
}

func translateText(text, sourceLang, targetLang string) (string, error) {
	if translator == nil {
		return "", fmt.Errorf("translator not initialized")
	}
	return translator.Translate(text, sourceLang, targetLang)
}
