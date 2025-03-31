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
	Password    string
	CreatedAt   time.Time
	Name        string
	Description string
	IsActive    bool
}

type Speaker struct {
	Conn       *websocket.Conn
	Language   string
	IsCreator  bool
	LastActive time.Time
}

type Audience struct {
	Conn       *websocket.Conn
	Language   string
	LastActive time.Time
}

var (
	streams = make(map[string]*Stream)
	mu      sync.RWMutex
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
		for password, stream := range streams {
			if stream.IsActive {
				activeStreams = append(activeStreams, map[string]interface{}{
					"password":    password,
					"name":        stream.Name,
					"description": stream.Description,
					"speakers":    len(stream.Speakers),
					"audience":    len(stream.Audience),
					"created_at":  stream.CreatedAt,
				})
			}
		}
		mu.RUnlock()

		log.Printf("Found %d active streams", len(activeStreams))
		for _, stream := range activeStreams {
			log.Printf("Stream: %s, Name: %s, Speakers: %d, Audience: %d",
				stream["password"], stream["name"], stream["speakers"], stream["audience"])
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

func getOrCreateStream(password string) *Stream {
	mu.Lock()
	defer mu.Unlock()

	// Check if stream exists
	if stream, exists := streams[password]; exists {
		log.Printf("Found existing stream with password: %s", password)
		return stream
	}

	// Create new stream
	log.Printf("Creating new stream with password: %s", password)
	streams[password] = &Stream{
		Name:        "",
		Description: "",
		Speakers:    make(map[string]*Speaker),
		Audience:    make(map[string]*Audience),
		IsActive:    true,
		CreatedAt:   time.Now(),
	}
	return streams[password]
}

func handleWebSocket(c *gin.Context) {
	role := c.Query("role")
	lang := c.Query("lang")
	password := c.Query("password")
	isCreator := c.Query("isCreator") == "true"
	streamName := c.Query("name")
	streamDesc := c.Query("description")

	log.Printf("WebSocket connection request - Role: %s, Password: %s, Creator: %v, Name: %s", role, password, isCreator, streamName)

	if password == "" {
		log.Printf("Error: Password is required")
		c.JSON(http.StatusBadRequest, gin.H{"error": "Password is required"})
		return
	}

	// Get or create stream before WebSocket upgrade
	stream := getOrCreateStream(password)
	mu.Lock()
	defer mu.Unlock()

	// If joining an existing stream, verify it exists and is active
	if !isCreator {
		if !stream.IsActive {
			log.Printf("Attempted to join inactive stream: %s", password)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Stream is not active"})
			return
		}
		log.Printf("Joining existing stream: %s", password)
	} else {
		// For new streams, ensure the stream name is unique
		if streamName != "" {
			for _, s := range streams {
				if s.Name == streamName && s != stream {
					log.Printf("Stream name already in use: %s", streamName)
					c.JSON(http.StatusBadRequest, gin.H{"error": "Stream name already in use"})
					return
				}
			}
		}
		log.Printf("Creating new stream: %s", password)
	}

	// Update stream info if creator
	if isCreator {
		if streamName != "" {
			stream.Name = streamName
			log.Printf("Setting stream name to: %s", streamName)
		} else {
			stream.Name = "New Stream"
			log.Printf("Using default stream name: New Stream")
		}
		if streamDesc != "" {
			stream.Description = streamDesc
			log.Printf("Setting stream description to: %s", streamDesc)
		}
		stream.IsActive = true
		stream.Password = password
		log.Printf("Stream created: %s (Name: %s, Password: %s)", password, stream.Name, stream.Password)
	}

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
		"isCreator":  isCreator,
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
			IsCreator:  isCreator,
			LastActive: time.Now(),
		}
		stream.Speakers[speakerID] = speaker
		log.Printf("Added speaker %s to stream %s (Creator: %v)", speakerID, password, isCreator)
		defer func() {
			conn.Close()
			delete(stream.Speakers, speakerID)
			log.Printf("Speaker %s left stream %s", speakerID, password)
			if len(stream.Speakers) == 0 && len(stream.Audience) == 0 {
				stream.IsActive = false
				log.Printf("Stream %s is now inactive", password)
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
		log.Printf("Added audience member %s to stream %s", audienceID, password)
		defer func() {
			conn.Close()
			delete(stream.Audience, audienceID)
			log.Printf("Audience member %s left stream %s", audienceID, password)
			if len(stream.Speakers) == 0 && len(stream.Audience) == 0 {
				stream.IsActive = false
				log.Printf("Stream %s is now inactive", password)
			}
		}()
	}

	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket read error: %v", err)
			}
			break
		}

		if messageType == websocket.TextMessage {
			var data map[string]interface{}
			if err := json.Unmarshal(message, &data); err != nil {
				log.Printf("JSON unmarshal error: %v", err)
				continue
			}

			if data["type"] == "speech" {
				text := data["text"].(string)
				speakerID := conn.RemoteAddr().String()
				speaker := stream.Speakers[speakerID]

				// Broadcast to all audience members
				for _, audience := range stream.Audience {
					if audience.Language != speaker.Language {
						translatedText, err := translateText(text, speaker.Language, audience.Language)
						if err != nil {
							log.Printf("Translation error: %v", err)
							continue
						}

						response := map[string]interface{}{
							"type": "translation",
							"text": translatedText,
						}
						responseJSON, _ := json.Marshal(response)
						audience.Conn.WriteMessage(websocket.TextMessage, responseJSON)
					}
				}
			} else if data["type"] == "password_update" {
				// Only allow the stream creator to update the password
				speakerID := conn.RemoteAddr().String()
				speaker := stream.Speakers[speakerID]
				if speaker != nil && speaker.IsCreator {
					newPassword := data["password"].(string)
					if newPassword != stream.Password {
						// Update the stream password
						stream.Password = newPassword
						// Notify all speakers and audience about the password update
						updateMsg := map[string]interface{}{
							"type":     "password_updated",
							"password": newPassword,
						}
						updateJSON, _ := json.Marshal(updateMsg)

						// Notify all speakers
						for _, s := range stream.Speakers {
							s.Conn.WriteMessage(websocket.TextMessage, updateJSON)
						}

						// Notify all audience members
						for _, a := range stream.Audience {
							a.Conn.WriteMessage(websocket.TextMessage, updateJSON)
						}
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
