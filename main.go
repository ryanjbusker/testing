package main

import (
	"context" //Added for OAuth
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"translation/translation"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/joho/godotenv"

	//The following four lines are added for OAuth
	// "github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/polly"
	"github.com/aws/aws-sdk-go-v2/service/polly/types"
	"github.com/aws/aws-sdk-go-v2/service/transcribestreaming"
	transcribeTypes "github.com/aws/aws-sdk-go-v2/service/transcribestreaming/types"
	"github.com/google/uuid"
)

var translator *translation.Translator
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins in development
	},
}

var pollyClient *polly.Client
var transcriptionManager *TranscriptionManager

type Stream struct {
	Speakers    map[string]*Speaker
	Audience    map[string]*Audience
	CreatedAt   time.Time
	Name        string
	Description string
	IsActive    bool
}

type Speaker struct {
	Conn            *websocket.Conn
	Language        string
	LastActive      time.Time
	TranscriptionID string // Added for transcription
	IsTranscribing  bool   // Added for transcription
}

type Audience struct {
	Conn       *websocket.Conn
	Language   string
	LastActive time.Time
}

// TranscriptionManager handles AWS Transcribe streaming sessions
type TranscriptionManager struct {
	client        *transcribestreaming.Client
	activeStreams map[string]*TranscriptionStream
	mu            sync.RWMutex
}

// TranscriptionStream represents an active transcription session
type TranscriptionStream struct {
	ID             string
	SpeakerID      string
	SourceLanguage string
	LastActive     time.Time
	EventStream    *transcribestreaming.StartStreamTranscriptionEventStream
	Context        context.Context
	Cancel         context.CancelFunc
}

// TranscriptEventHandler handles transcript events from AWS Transcribe
type TranscriptEventHandler struct {
	speakerID    string
	languageCode string
	buffer       string
	conn         *websocket.Conn
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

	//The following three lines have been added for OAuth
	oauthConfig      *oauth2.Config
	oauthStateString = "random-state-string"
	store            = sessions.NewCookieStore([]byte(os.Getenv("SESSION_KEY")))
)

func init() {
	// Load environment variables first
	if err := godotenv.Load(); err != nil {
		log.Println("Warning: .env file not found")
	}

	key := os.Getenv("SESSION_KEY")
	if key == "" {
		log.Fatal("SESSION_KEY is empty or not set in .env")
	}

	//Initialize session store using the loaded key
	store = sessions.NewCookieStore([]byte(key))
	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   86400 * 7, // 7 days
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode, // or SameSiteStrictMode
		Secure:   os.Getenv("ENV") == "production",
	}

	// Log AWS configuration values
	region := os.Getenv("TRANSLATE_REGION")
	accessKey := os.Getenv("TRANSLATE_ACCESS_KEY_ID")
	secretKey := os.Getenv("TRANSLATE_SECRET_ACCESS_KEY")

	log.Printf("AWS Region: %s", region)
	log.Printf("AWS Access Key ID length: %d", len(accessKey))
	log.Printf("AWS Secret Key length: %d", len(secretKey))

	// Initialize AWS Polly client
	awsCfg, awsErr := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			accessKey,
			secretKey,
			"",
		)),
	)
	if awsErr != nil {
		log.Fatalf("unable to load SDK config, %v", awsErr)
	}

	pollyClient = polly.NewFromConfig(awsCfg)
	log.Println("AWS Polly client initialized successfully")

	// Initialize translator
	var translatorErr error
	translator, translatorErr = translation.NewTranslator()
	if translatorErr != nil {
		log.Fatalf("Failed to initialize translator: %v", translatorErr)
	}

	// Initialize transcription manager
	var transcriptionErr error
	transcriptionManager, transcriptionErr = NewTranscriptionManager()
	if transcriptionErr != nil {
		log.Fatalf("Failed to initialize transcription manager: %v", transcriptionErr)
	}
	log.Println("Transcription manager initialized successfully")

	// Initialize OAuth config
	oauthConfig = &oauth2.Config{
		RedirectURL:  os.Getenv("OAUTH_REDIRECT_URL"),
		ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		Scopes:       []string{"https://www.googleapis.com/auth/userinfo.email"},
		Endpoint:     google.Endpoint,
	}
}

// Initialize AWS Transcribe client
func initTranscribeClient() (*transcribestreaming.Client, error) {
	log.Println("Initializing AWS Transcribe client...")
	
	region := os.Getenv("TRANSLATE_REGION")  // Use same region as your translation service
	accessKey := os.Getenv("TRANSLATE_ACCESS_KEY_ID")
	secretKey := os.Getenv("TRANSLATE_SECRET_ACCESS_KEY")
	
	if region == "" || accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("AWS credentials not properly configured for Transcribe")
	}
	
	awsCfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			accessKey,
			secretKey,
			"",
		)),
	)
	
	if err != nil {
		return nil, fmt.Errorf("unable to load AWS SDK config: %v", err)
	}
	
	// Create the Transcribe streaming client
	client := transcribestreaming.NewFromConfig(awsCfg)
	log.Println("AWS Transcribe client initialized successfully")
	
	return client, nil
}

// Create a new transcription manager with the AWS client
func NewTranscriptionManager() (*TranscriptionManager, error) {
	client, err := initTranscribeClient()
	if err != nil {
		return nil, err
	}
	
	return &TranscriptionManager{
		client:        client,
		activeStreams: make(map[string]*TranscriptionStream),
	}, nil
}

// StartTranscription starts a new streaming transcription session
func (m *TranscriptionManager) StartTranscription(speakerID, language string, conn *websocket.Conn) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	// Generate a unique ID for this transcription session
	sessionID := uuid.New().String()
	
	// Create a cancelable context for this stream
	ctx, cancel := context.WithCancel(context.Background())
	
	// Map AWS language codes from our simplified codes
	languageCode := mapToAWSLanguageCode(language)
	
	// Set up the start stream transcription request
	input := &transcribestreaming.StartStreamTranscriptionInput{
		LanguageCode:         transcribeTypes.LanguageCode(languageCode),
		MediaEncoding:        transcribeTypes.MediaEncodingOggOpus,  // We'll use Opus for good quality
		MediaSampleRateHertz: aws.Int32(48000),                      // 48kHz is good for voice
		EnablePartialResultsStabilization: true,
		PartialResultsStability:          transcribeTypes.PartialResultsStabilityHigh,
	}
	
	// Create event handler for results
	handler := &TranscriptEventHandler{
		speakerID:    speakerID,
		languageCode: language,
		conn:         conn,
	}
	
	// Start the stream transcription
	stream, err := m.client.StartStreamTranscription(ctx, input)
	if err != nil {
		cancel() // Cancel the context if we fail to start
		return "", fmt.Errorf("failed to start transcription stream: %v", err)
	}
	
	// Store the active stream
	m.activeStreams[sessionID] = &TranscriptionStream{
		ID:             sessionID,
		SpeakerID:      speakerID,
		SourceLanguage: language,
		LastActive:     time.Now(),
		EventStream:    stream,
		Context:        ctx,
		Cancel:         cancel,
	}
	
	// Start a goroutine to process the events from AWS
	go m.processTranscriptionEvents(sessionID, stream, handler)
	
	log.Printf("Started transcription for speaker %s with ID %s", speakerID, sessionID)
	return sessionID, nil
}

// StopTranscription stops an active transcription stream
func (m *TranscriptionManager) StopTranscription(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	stream, exists := m.activeStreams[sessionID]
	if !exists {
		return fmt.Errorf("transcription stream not found: %s", sessionID)
	}
	
	// Cancel the context and close the stream
	stream.Cancel()
	
	// Remove from active streams
	delete(m.activeStreams, sessionID)
	
	log.Printf("Stopped transcription stream %s", sessionID)
	return nil
}

// ProcessAudioChunk sends audio data to AWS Transcribe
func (m *TranscriptionManager) ProcessAudioChunk(sessionID string, audioData []byte) error {
	m.mu.RLock()
	stream, exists := m.activeStreams[sessionID]
	if !exists {
		m.mu.RUnlock()
		return fmt.Errorf("transcription stream not found: %s", sessionID)
	}
	
	// Update the last active time
	stream.LastActive = time.Now()
	eventStream := stream.EventStream
	m.mu.RUnlock()
	
	// Send the audio chunk to AWS
	audioEvent := transcribeTypes.AudioEvent{
		AudioChunk: audioData,
	}
	
	err := eventStream.Send(context.Background(), &transcribestreaming.AudioStreamEvent{
		AudioEvent: &audioEvent,
	})
	
	if err != nil {
		log.Printf("Error sending audio chunk to AWS: %v", err)
		return err
	}
	
	return nil
}

// processTranscriptionEvents handles the stream of events from AWS Transcribe
func (m *TranscriptionManager) processTranscriptionEvents(
	sessionID string, 
	stream *transcribestreaming.StartStreamTranscriptionEventStream, 
	handler *TranscriptEventHandler) {
	
	defer func() {
		// Clean up when the processing is done
		m.mu.Lock()
		delete(m.activeStreams, sessionID)
		m.mu.Unlock()
		log.Printf("Transcription event processing ended for session %s", sessionID)
	}()
	
	for {
		event, err := stream.Events.Recv(context.Background())
		if err != nil {
			// Check if it's just the context being canceled
			if m.isStreamActive(sessionID) {
				log.Printf("Error receiving transcription events: %v", err)
			}
			return
		}
		
		// Process different event types
		switch v := event.(type) {
		case *transcribeTypes.TranscriptEvent:
			handler.handleTranscriptEvent(v)
		}
	}
}

// isStreamActive checks if a stream is still active
func (m *TranscriptionManager) isStreamActive(sessionID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, exists := m.activeStreams[sessionID]
	return exists
}

// handleTranscriptEvent processes transcript events and sends them to the speaker's websocket
func (h *TranscriptEventHandler) handleTranscriptEvent(event *transcribeTypes.TranscriptEvent) {
	if event == nil || len(event.Transcript.Results) == 0 {
		return
	}
	
	for _, result := range event.Transcript.Results {
		if len(result.Alternatives) == 0 {
			continue
		}
		
		// Get the transcript text
		transcript := result.Alternatives[0].Transcript
		
		// If this is a final result, send it through the translation pipeline
		if result.IsPartial == nil || !*result.IsPartial {
			// Format the recognized text message
			recognizedTextMsg := map[string]interface{}{
				"type":     "recognizedText",
				"text":     transcript,
				"isFinal":  true,
			}
			
			// Send the recognized text back to the speaker
			recognizedJSON, _ := json.Marshal(recognizedTextMsg)
			h.conn.WriteMessage(websocket.TextMessage, recognizedJSON)
			
			// Now send it through the translation pipeline
			speechMsg := map[string]interface{}{
				"type":     "speech",
				"text":     transcript,
				"language": h.languageCode,
			}
			
			speechJSON, _ := json.Marshal(speechMsg)
			h.conn.WriteMessage(websocket.TextMessage, speechJSON)
		} else {
			// For partial results, just update the UI
			partialMsg := map[string]interface{}{
				"type":     "recognizedText",
				"text":     transcript,
				"isFinal":  false,
			}
			
			partialJSON, _ := json.Marshal(partialMsg)
			h.conn.WriteMessage(websocket.TextMessage, partialJSON)
		}
	}
}

// mapToAWSLanguageCode converts our language codes to AWS language codes
func mapToAWSLanguageCode(langCode string) string {
	// Mapping of our simplified codes to AWS language codes
	langMap := map[string]string{
		"en": "en-US",
		"ar": "ar-SA",
		"zh": "zh-CN",
		"da": "da-DK",
		"nl": "nl-NL",
		"fr": "fr-FR",
		"de": "de-DE",
		"hi": "hi-IN",
		"it": "it-IT",
		"ja": "ja-JP",
		"ko": "ko-KR",
		"pl": "pl-PL",
		"pt": "pt-BR",
		"ro": "ro-RO", 
		"ru": "ru-RU",
		"es": "es-ES",
		"sv": "sv-SE",
		"tr": "tr-TR",
	}
	
	if awsCode, exists := langMap[langCode]; exists {
		return awsCode
	}
	
	// Default to English if not found
	return "en-US"
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
	// router.LoadHTMLGlob("templates/**/*.html")
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

	//The folllowing router.GET was added for OAuth
	router.GET("/login", func(c *gin.Context) {
		url := oauthConfig.AuthCodeURL(oauthStateString)
		c.Redirect(http.StatusTemporaryRedirect, url)
	})

	router.GET("/callback", func(c *gin.Context) {
		if c.Query("state") != oauthStateString {
			c.String(http.StatusBadRequest, "State mismatch")
			return
		}

		token, err := oauthConfig.Exchange(context.Background(), c.Query("code"))
		if err != nil {
			log.Printf("Token exchange failed: %v", err)
			c.String(http.StatusInternalServerError, "Token exchange failed")
			return
		}

		client := oauthConfig.Client(context.Background(), token)
		emailResp, err := client.Get("https://www.googleapis.com/oauth2/v2/userinfo")
		if err != nil {
			log.Printf("Failed getting user info: %v", err)
			c.String(http.StatusInternalServerError, "Failed getting user info")
			return
		}
		defer emailResp.Body.Close()

		// Extract email
		email := extractEmail(emailResp)
		if email == "" {
			log.Println("Email not found in user info response")
			c.String(http.StatusInternalServerError, "Failed to extract email")
			return
		}

		log.Printf("User logged in with email: %s", email)

		// Save email to session
		session, _ := store.Get(c.Request, "session-name")
		session.Values["email"] = email

		// Set session cookie properties (optional but recommended)
		store.Options = &sessions.Options{
			Path:     "/",
			MaxAge:   86400 * 7, // 7 days
			HttpOnly: true,
			Secure:   false, // set to true in production with HTTPS
			SameSite: http.SameSiteLaxMode,
		}

		// Save the session
		err = session.Save(c.Request, c.Writer)
		if err != nil {
			log.Printf("Failed to save session: %v", err)
			c.String(http.StatusInternalServerError, "Failed to save session")
			return
		}

		// Redirect back to the original page, if present
		from := c.Query("from")
		if from == "" {
			from = "/" // Default to home if nothing specified
		}
		log.Printf("Redirecting user to: %s", from)
		c.Redirect(http.StatusSeeOther, from)
	})

	router.GET("/logout", func(c *gin.Context) {
		session, _ := store.Get(c.Request, "session-name")
		delete(session.Values, "email")
		session.Save(c.Request, c.Writer)
		c.Redirect(http.StatusSeeOther, "/")
	})

	// router.GET("/account", func(c *gin.Context) {
	//  session, _ := store.Get(c.Request, "session-name")
	//  email, ok := session.Values["email"].(string)
	//  if !ok || email == "" {
	//      c.Redirect(http.StatusSeeOther, "/")
	//      return
	//  }
	//  c.File("templates/account.html")
	// })
	router.GET("/account", func(c *gin.Context) {
		log.Printf("Serving account.html")
		c.HTML(http.StatusOK, "account.html", gin.H{
			"title": "Account",
		})
	})

	router.GET("/session", func(c *gin.Context) {
		session, _ := store.Get(c.Request, "session-name")
		email := session.Values["email"]
		c.JSON(http.StatusOK, gin.H{"email": email})
	})

	// Add Polly TTS endpoint
	router.POST("/polly-tts", handlePollyTTS)

	// Get port from environment variable or use default
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Start server
	log.Printf("Server starting on port %s", port)
	if err := router.Run("0.0.0.0:" + port); err != nil {
		log.Fatal("Failed to start server:", err)
	}
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
			Conn:           conn,
			Language:       lang,
			LastActive:     time.Now(),
			IsTranscribing: false,    // Initialize as not transcribing
		}
		stream.Speakers[speakerID] = speaker
		log.Printf("Added speaker %s", speakerID)
		
		defer func() {
			// If transcription is active, stop it
			if speaker.IsTranscribing && speaker.TranscriptionID != "" {
				transcriptionManager.StopTranscription(speaker.TranscriptionID)
				log.Printf("Stopped transcription for speaker %s", speakerID)
			}
			
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
		mu.Lock() // Use write lock to modify the stream
		stream.Audience[audienceID] = audience
		log.Printf("Added audience member %s", audienceID)
		mu.Unlock()

		defer func() {
			mu.Lock() // Use write lock to modify the stream
			conn.Close()
			delete(stream.Audience, audienceID)
			log.Printf("Audience member %s left", audienceID)
			if len(stream.Speakers) == 0 && len(stream.Audience) == 0 {
				stream.IsActive = false
				log.Printf("Stream is now inactive")
			}
			mu.Unlock()
		}() // Ensure the function call syntax () is present
	}

	// Handle incoming messages
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Error reading message: %v", err)
			}
			break // Exit loop on error or close
		}

		speakerID := conn.RemoteAddr().String()

		// Handle different message types based on content
		if messageType == websocket.TextMessage {
			var data map[string]interface{}
			if err := json.Unmarshal(message, &data); err != nil {
				log.Printf("Error parsing message: %v", err)
				continue
			}

			msgType, ok := data["type"].(string)
			if !ok {
				log.Printf("Message missing 'type' field")
				continue
			}

			switch msgType {
			case "speech":
				// Handle text message (existing code for manual text entry)
				handleSpeechMessage(conn, data, speakerID)
				
			case "startTranscription":
				// Handle request to start transcription
				mu.Lock()
				speaker, exists := stream.Speakers[speakerID]
				if !exists {
					mu.Unlock()
					log.Printf("Start transcription requested from unknown speaker: %s", speakerID)
					continue
				}
				
				if speaker.IsTranscribing {
					// Already transcribing, stop the existing one first
					transcriptionManager.StopTranscription(speaker.TranscriptionID)
				}
				
				// Start a new transcription stream
				transcriptionID, err := transcriptionManager.StartTranscription(speakerID, speaker.Language, speaker.Conn)
				if err != nil {
					log.Printf("Failed to start transcription: %v", err)
					errorMsg := map[string]interface{}{
						"type":  "error",
						"error": "Failed to start transcription",
					}
					errorJSON, _ := json.Marshal(errorMsg)
					speaker.Conn.WriteMessage(websocket.TextMessage, errorJSON)
					mu.Unlock()
					continue
				}
				
				speaker.TranscriptionID = transcriptionID
				speaker.IsTranscribing = true
				mu.Unlock()
				
				// Send confirmation to client
				startedMsg := map[string]interface{}{
					"type": "transcriptionStarted",
					"id":   transcriptionID,
				}
				startedJSON, _ := json.Marshal(startedMsg)
				conn.WriteMessage(websocket.TextMessage, startedJSON)
				
				log.Printf("Started transcription for speaker %s with ID %s", speakerID, transcriptionID)
				
			case "stopTranscription":
				// Handle request to stop transcription
				mu.Lock()
				speaker, exists := stream.Speakers[speakerID]
				if !exists || !speaker.IsTranscribing {
					mu.Unlock()
					log.Printf("Stop transcription requested but no active transcription for speaker: %s", speakerID)
					continue
				}
				
				err := transcriptionManager.StopTranscription(speaker.TranscriptionID)
				if err != nil {
					log.Printf("Error stopping transcription: %v", err)
				}
				
				speaker.IsTranscribing = false
				speaker.TranscriptionID = ""
				mu.Unlock()
				
				// Send confirmation to client
				stoppedMsg := map[string]interface{}{
					"type": "transcriptionStopped",
				}
				stoppedJSON, _ := json.Marshal(stoppedMsg)
				conn.WriteMessage(websocket.TextMessage, stoppedJSON)
				
				log.Printf("Stopped transcription for speaker %s", speakerID)
				
			case "audioChunk":
				// Handle incoming audio data for transcription
				mu.RLock()
				speaker, exists := stream.Speakers[speakerID]
				if !exists || !speaker.IsTranscribing {
					mu.RUnlock()
					log.Printf("Audio chunk received but no active transcription for speaker: %s", speakerID)
					continue
				}
				
				transcriptionID := speaker.TranscriptionID
				mu.RUnlock()
				
				// Get the base64 encoded audio data
				audioBase64, ok := data["audio"].(string)
				if !ok {
					log.Printf("Audio chunk missing 'audio' field")
					continue
				}
				
				// Decode the base64 audio data
				audioData, err := base64.StdEncoding.DecodeString(audioBase64)
				if err != nil {
					log.Printf("Failed to decode audio data: %v", err)
					continue
				}
				
				// Process the audio chunk
				err = transcriptionManager.ProcessAudioChunk(transcriptionID, audioData)
				if err != nil {
					log.Printf("Error processing audio chunk: %v", err)
				}
			
			default:
				log.Printf("Unknown message type: %s", msgType)
			}
		} else if messageType == websocket.BinaryMessage {
			// Handle binary messages (direct audio data without JSON wrapper)
			mu.RLock()
			speaker, exists := stream.Speakers[speakerID]
			if !exists || !speaker.IsTranscribing {
				mu.RUnlock()
				log.Printf("Binary audio received but no active transcription for speaker: %s", speakerID)
				continue
			}
			
			transcriptionID := speaker.TranscriptionID
			mu.RUnlock()
			
			// Process the audio chunk directly
			err = transcriptionManager.ProcessAudioChunk(transcriptionID, message)
			if err != nil {
				log.Printf("Error processing binary audio chunk: %v", err)
			}
		} else {
			log.Printf("Received unsupported message type: %d", messageType)
		}
	}
}

// Handle speech message (unchanged from your original code)
func handleSpeechMessage(conn *websocket.Conn, data map[string]interface{}, speakerID string) {
	text, ok := data["text"].(string)
	if !ok || text == "" {
		log.Printf("Received speech message with invalid or empty text")
		return
	}

	mu.RLock() // Lock for reading speaker and audience data
	speaker := stream.Speakers[speakerID]

	if speaker == nil {
		mu.RUnlock()
		log.Printf("Received speech message from unknown speaker: %s", speakerID)
		return // Ignore message if speaker is not found (already disconnected?)
	}

	// Group audience by target language
	audienceByLang := make(map[string][]*websocket.Conn)
	for _, audience := range stream.Audience {
		audienceByLang[audience.Language] = append(audienceByLang[audience.Language], audience.Conn)
	}
	mu.RUnlock() // Unlock after reading audience data

	// Translate once per target language
	translations := make(map[string]string)
	translationErrors := make(map[string]error)
	for targetLang := range audienceByLang {
		translatedText, err := translateText(text, speaker.Language, targetLang)
		if err != nil {
			log.Printf("Translation error from %s to %s: %v", speaker.Language, targetLang, err)
			translationErrors[targetLang] = err // Store error
			continue                            // Skip this language if translation fails
		}
		translations[targetLang] = translatedText
		log.Printf("Translated '%s' (%s) to '%s' (%s)", text, speaker.Language, translatedText, targetLang) // Log successful translation
	}

	// Send translated text to relevant audience groups
	for targetLang, conns := range audienceByLang {
		// Check if translation was successful for this language
		translatedText, ok := translations[targetLang]
		if !ok {
			// Optionally send an error message to these clients
			// log.Printf("Skipping sending to %s due to translation error: %v", targetLang, translationErrors[targetLang])
			continue
		}

		response := map[string]interface{}{
			"type":     "translation",
			"text":     translatedText,
			"speaker":  speakerID,
			"original": text, // Include original text for context if needed
		}
		responseJSON, err := json.Marshal(response)
		if err != nil {
			log.Printf("Error marshalling translation response: %v", err)
			continue // Skip this group if marshalling fails
		}

		// Send to all connections in this language group
		for _, audienceConn := range conns {
			if err := audienceConn.WriteMessage(websocket.TextMessage, responseJSON); err != nil {
				log.Printf("Error sending translation to audience %s: %v", audienceConn.RemoteAddr().String(), err)
				// Handle potential write errors (e.g., remove disconnected client)
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

func extractEmail(resp *http.Response) string {
	var userInfo struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
		log.Printf("Error decoding user info: %v", err)
		return ""
	}
	return userInfo.Email
}

func handlePollyTTS(c *gin.Context) {
	log.Println("=== POLLY TTS ENDPOINT CALLED ===")

	var req struct {
		Text     string `json:"text"`
		Language string `json:"language"`
		VoiceId  string `json:"voiceId"`
	}
	if err := c.BindJSON(&req); err != nil {
		log.Printf("Error binding JSON request: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	// Log the incoming request
	log.Printf("Polly TTS Request - Text: %q, Language: %s, VoiceID: %s", req.Text, req.Language, req.VoiceId)

	voiceId := types.VoiceId("Joanna")
	if req.VoiceId != "" {
		voiceId = types.VoiceId(req.VoiceId)
	} else {
		// Otherwise, select voice based on language
		switch req.Language {
		case "es-ES":
			voiceId = types.VoiceId("Lucia")
		case "fr-FR":
			voiceId = types.VoiceId("Lea")
		case "de-DE":
			voiceId = types.VoiceId("Vicki")
		case "it-IT":
			voiceId = types.VoiceId("Carla")
		case "pt-BR":
			voiceId = types.VoiceId("Camila")
		case "nl-NL":
			voiceId = types.VoiceId("Laura")
		case "pl-PL":
			voiceId = types.VoiceId("Ola")
		case "ru-RU":
			voiceId = types.VoiceId("Tatyana")
		case "ja-JP":
			voiceId = types.VoiceId("Takumi")
		case "ko-KR":
			voiceId = types.VoiceId("Seoyeon")
		case "zh-CN":
			voiceId = types.VoiceId("Zhiyu")
		}
	}

	log.Printf("Selected voice ID: %s", voiceId)

	input := &polly.SynthesizeSpeechInput{
		Text:         aws.String(req.Text),
		OutputFormat: types.OutputFormatMp3,
		VoiceId:      voiceId,
		Engine:       types.EngineNeural,
	}

	log.Printf("Sending request to Polly with input: %+v", input)

	output, err := pollyClient.SynthesizeSpeech(context.Background(), input)
	if err != nil {
		log.Printf("Error synthesizing speech: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to synthesize speech"})
		return
	}

	log.Printf("Successfully received response from Polly")

	// Set headers for audio streaming
	c.Header("Content-Type", "audio/mpeg")
	c.Header("Content-Disposition", "attachment; filename=speech.mp3")
	c.Header("Transfer-Encoding", "chunked")

	// Stream the audio data to the client
	defer output.AudioStream.Close()
	_, err = io.Copy(c.Writer, output.AudioStream)
	if err != nil {
		log.Printf("Error streaming audio: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to stream audio"})
		return
	}

	log.Printf("Successfully streamed audio to client")
	log.Println("=== POLLY TTS ENDPOINT COMPLETED ===")
}