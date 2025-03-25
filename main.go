package main

import (
	"log"
	"net/http"
	"os"

	"translation/translation"
	"translation/websocket"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

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

func main() {
	router := gin.Default()

	// Serve static files
	router.Static("/static", "./static")

	// Load all HTML templates
	router.LoadHTMLGlob("templates/*.html")

	// Routes
	router.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", gin.H{
			"title": "Translation Service",
		})
	})

	router.GET("/speaker", func(c *gin.Context) {
		c.HTML(http.StatusOK, "speaker.html", gin.H{
			"title": "Speaker Page",
		})
	})

	router.GET("/audience", func(c *gin.Context) {
		c.HTML(http.StatusOK, "audience.html", gin.H{
			"title": "Audience Page",
		})
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
