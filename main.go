package main

import (
	"context" //Added for OAuth
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

	// Add Deepgram SDK imports
	dgclient "github.com/deepgram/deepgram-go-sdk/pkg/client/listen/v1/websocket"
	clientinterfaces "github.com/deepgram/deepgram-go-sdk/pkg/client/interfaces"
	msginterfaces "github.com/deepgram/deepgram-go-sdk/pkg/api/listen/v1/websocket/interfaces"
	client "github.com/deepgram/deepgram-go-sdk/pkg/client/listen"
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

// Add Deepgram API key
var deepgramAPIKey string

type Stream struct {
	Speakers    map[string]*Speaker
	Audience    map[string]*Audience
	CreatedAt   time.Time
	Name        string
	Description string
	IsActive    bool
}

type Speaker struct {
	Conn              *websocket.Conn
	Language          string
	LastActive        time.Time
	DeepgramClient    *dgclient.WSCallback // Add Deepgram client for each speaker
	DeepgramCtx       context.Context
	DeepgramCancelCtx context.CancelFunc
}

type Audience struct {
	Conn       *websocket.Conn
	Language   string
	LastActive time.Time
	SpeakerID  string // <- add this field
}

// Implement Deepgram callback interface
type DeepgramCallback struct {
	SourceLang  string
	SpeakerID   string
	SpeakerConn *websocket.Conn
	sb          *strings.Builder  // String builder to accumulate transcription
}

// Message implements the LiveMessageCallback interface for handling message responses
func (cb *DeepgramCallback) Message(mr *msginterfaces.MessageResponse) error {
	// Skip empty transcripts
	sentence := strings.TrimSpace(mr.Channel.Alternatives[0].Transcript)
	if len(mr.Channel.Alternatives) == 0 || len(sentence) == 0 {
		return nil
	}

	// Process the transcription
	log.Printf("[Deepgram] Transcription: %s (Final: %v)", sentence, mr.IsFinal)

	if mr.IsFinal {
		// Add to the string builder
		cb.sb.WriteString(sentence)
		cb.sb.WriteString(" ")

		// When speech is final, send the complete transcription
		if mr.SpeechFinal {
			completedText := cb.sb.String()
			log.Printf("[Deepgram] Final speech: %s", completedText)
			
			// Send the transcript to the speaker
			speechMsg := map[string]interface{}{
				"type":     "transcription",
				"text":     completedText,
				"language": cb.SourceLang,
			}
			speechJSON, _ := json.Marshal(speechMsg)
			if err := cb.SpeakerConn.WriteMessage(websocket.TextMessage, speechJSON); err != nil {
				log.Printf("Failed to send transcription back to speaker: %v", err)
			}
			
			// Process translations for audience members
			cb.processTranslations(completedText)
			
			// Reset the buffer for the next utterance
			cb.sb.Reset()
		}
	} else {
		// For interim results, just log them
		log.Printf("[Deepgram] Interim result: %s", sentence)
		
		// Optionally send interim results to the speaker
		// This would let them see partial transcriptions as they speak
		interimMsg := map[string]interface{}{
			"type":     "interim",
			"text":     sentence,
			"language": cb.SourceLang,
		}
		interimJSON, _ := json.Marshal(interimMsg)
		if err := cb.SpeakerConn.WriteMessage(websocket.TextMessage, interimJSON); err != nil {
			log.Printf("Failed to send interim transcription to speaker: %v", err)
		}
	}

	return nil
}

// Open implements the LiveMessageCallback interface
func (cb *DeepgramCallback) Open(ocr *msginterfaces.OpenResponse) error {
	log.Printf("[Deepgram] Connection opened for speaker: %s", cb.SpeakerID)
	return nil
}

// Metadata implements the LiveMessageCallback interface
func (cb *DeepgramCallback) Metadata(md *msginterfaces.MetadataResponse) error {
	log.Printf("[Deepgram] Metadata received - RequestID: %s, Channels: %d", 
		strings.TrimSpace(md.RequestID), md.Channels)
	return nil
}

// SpeechStarted implements the LiveMessageCallback interface
func (cb *DeepgramCallback) SpeechStarted(ssr *msginterfaces.SpeechStartedResponse) error {
	log.Printf("[Deepgram] Speech started for speaker: %s", cb.SpeakerID)
	return nil
}

// UtteranceEnd implements the LiveMessageCallback interface
func (cb *DeepgramCallback) UtteranceEnd(ur *msginterfaces.UtteranceEndResponse) error {
	utterance := strings.TrimSpace(cb.sb.String())
	if len(utterance) > 0 {
		log.Printf("[Deepgram] Utterance end: %s", utterance)
		
		// Send the final utterance to the speaker
		utteranceMsg := map[string]interface{}{
			"type":     "transcription",
			"text":     utterance,
			"language": cb.SourceLang,
			"final":    true,
		}
		utteranceJSON, _ := json.Marshal(utteranceMsg)
		if err := cb.SpeakerConn.WriteMessage(websocket.TextMessage, utteranceJSON); err != nil {
			log.Printf("Failed to send utterance to speaker: %v", err)
		}
		
		// Process translations for the audience
		cb.processTranslations(utterance)
		
		// Reset the buffer for the next utterance
		cb.sb.Reset()
	} else {
		log.Printf("[Deepgram] Empty utterance end received")
	}
	return nil
}

// Close implements the LiveMessageCallback interface
func (cb *DeepgramCallback) Close(closeResponse *msginterfaces.CloseResponse) error {
	log.Printf("[Deepgram] Connection closed for speaker: %s", cb.SpeakerID)
	return nil
}

// Error implements the LiveMessageCallback interface for handling error responses
func (cb *DeepgramCallback) Error(errorResponse *msginterfaces.ErrorResponse) error {
	log.Printf("[Deepgram] Error received - Type: %s, Code: %s, Description: %s", 
		errorResponse.Type, errorResponse.ErrCode, errorResponse.Description)
	return nil
}

// UnhandledEvent implements the LiveMessageCallback interface
func (cb *DeepgramCallback) UnhandledEvent(byData []byte) error {
	log.Printf("[Deepgram] Unhandled event received: %s", string(byData))
	return nil
}

// Helper method to process translations for all audience members
func (cb *DeepgramCallback) processTranslations(text string) {
	mu.RLock()
	// Group audience by target language
	audienceByLang := make(map[string][]*websocket.Conn)
	for _, audience := range stream.Audience {
		if audience.SpeakerID == cb.SpeakerID {
			audienceByLang[audience.Language] = append(audienceByLang[audience.Language], audience.Conn)
		}
	}
	mu.RUnlock()
	
	// Translate once per target language
	translations := make(map[string]string)
	translationErrors := make(map[string]error)
	for targetLang := range audienceByLang {
		translatedText, err := translateText(text, cb.SourceLang, targetLang)
		if err != nil {
			log.Printf("Translation error from %s to %s: %v", cb.SourceLang, targetLang, err)
			translationErrors[targetLang] = err
			continue
		}
		translations[targetLang] = translatedText
		log.Printf("Translated '%s' (%s) to '%s' (%s)", text, cb.SourceLang, translatedText, targetLang)
	}
	
	// Send translated text to relevant audience groups
	for targetLang, conns := range audienceByLang {
		translatedText, ok := translations[targetLang]
		if !ok {
			continue
		}
		
		response := map[string]interface{}{
			"type":     "translation",
			"text":     translatedText,
			"speaker":  cb.SpeakerID,
			"original": text,
		}
		responseJSON, err := json.Marshal(response)
		if err != nil {
			log.Printf("Error marshalling translation response: %v", err)
			continue
		}
		
		// Send to all connections in this language group
		for _, audienceConn := range conns {
			if err := audienceConn.WriteMessage(websocket.TextMessage, responseJSON); err != nil {
				log.Printf("Error sending translation to audience %s: %v", audienceConn.RemoteAddr().String(), err)
			}
		}
	}
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

	// Get Deepgram API key
	deepgramAPIKey = os.Getenv("DEEPGRAM_API_KEY")
	if deepgramAPIKey == "" {
		log.Println("Warning: DEEPGRAM_API_KEY is not set in .env")
	} else {
		log.Println("Deepgram API key loaded successfully")
	}
	
	// Initialize the Deepgram client
	client.Init(client.InitLib{
		LogLevel: client.LogLevelDefault, // Can be LogLevelDefault, LogLevelFull, LogLevelDebug, LogLevelTrace
	})
	log.Println("Deepgram client initialized successfully")

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

	// Initialize OAuth config
	oauthConfig = &oauth2.Config{
		RedirectURL:  os.Getenv("OAUTH_REDIRECT_URL"),
		ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		Scopes:       []string{"https://www.googleapis.com/auth/userinfo.email"},
		Endpoint:     google.Endpoint,
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
	speakerID := c.Query("id") // For speakers
	// audienceSpeakerID := c.Query("speaker") // For audience selecting a speaker

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
		// speakerID := conn.RemoteAddr().String()
		speakerID := c.Query("id")
		if speakerID == "" {
			log.Printf("Missing speaker ID in query")
			conn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Missing speaker ID"}`))
			conn.Close()
			return
		}
		mu.RLock()
		_, exists := stream.Speakers[speakerID]
		mu.RUnlock()

		if exists {
			log.Printf("Speaker ID %s is already in use", speakerID)
			conn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Speaker ID already in use"}`))
			conn.Close()
			return
		}


		
		// Create context for Deepgram client
		ctx, cancel := context.WithCancel(context.Background())
		
		// Set up Deepgram transcription options
		transcriptionOptions := &clientinterfaces.LiveTranscriptionOptions{
			Language:        lang,             // Language from client
			Model:           "nova-2",         // Use Nova-2 model
			Punctuate:       true,             // Add punctuation
			Encoding:        "linear16",       // Linear PCM encoding
			SampleRate:      16000,            // 16kHz sample rate
			Channels:        1,                // Mono audio
			SmartFormat:     true,             // Apply smart formatting
			InterimResults:  true,             // Get intermediate results
			UtteranceEndMs:  "1000",           // End utterance after 1 second of silence
			VadEvents:       true,             // Voice activity detection events
		}
		
		// Create Deepgram callback with string builder for accumulating text
		callback := &DeepgramCallback{
			SourceLang:  lang,
			SpeakerID:   speakerID,
			SpeakerConn: conn,
			sb:          &strings.Builder{},
		}
		
		// Client options
		clientOptions := &clientinterfaces.ClientOptions{
			EnableKeepAlive: true,  // Keep the connection alive
		}
		
		// Initialize Deepgram client
		deepgramClient, err := client.NewWSUsingCallbackWithCancel(
			ctx,
			cancel,
			deepgramAPIKey,
			clientOptions,
			transcriptionOptions,
			callback,
		)
		
		if err != nil {
			log.Printf("Failed to create Deepgram client: %v", err)
			conn.Close()
			return
		}
		
		// Connect to Deepgram WebSocket API
		connected := deepgramClient.Connect()
		if !connected {
			log.Printf("Failed to connect to Deepgram WebSocket API")
			cancel()
			conn.Close()
			return
		}
		
		log.Printf("Connected to Deepgram WebSocket for speaker %s", speakerID)
		
		speaker := &Speaker{
			Conn:              conn,
			Language:          lang,
			LastActive:        time.Now(),
			DeepgramClient:    deepgramClient,
			DeepgramCtx:       ctx,
			DeepgramCancelCtx: cancel,
		}
		
		mu.Lock()
		stream.Speakers[speakerID] = speaker
		mu.Unlock()
		
		log.Printf("Added speaker %s", speakerID)
		
		defer func() {
			// Close Deepgram connection
			deepgramClient.Stop()
			cancel()
			
			// Close WebSocket connection
			conn.Close()
			
			// Remove speaker from stream
			mu.Lock()
			delete(stream.Speakers, speakerID)
			mu.Unlock()
			
			log.Printf("Speaker %s left", speakerID)
			
			// Check if stream is still active
			mu.RLock()
			if len(stream.Speakers) == 0 && len(stream.Audience) == 0 {
				stream.IsActive = false
				log.Printf("Stream is now inactive")
			}
			mu.RUnlock()
			
			// Notify others that this speaker left
			mu.RLock()
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
			mu.RUnlock()
		}()
	} else if role == "audience" {
		audienceID := conn.RemoteAddr().String()
		audience := &Audience{
			Conn:       conn,
			Language:   lang,
			LastActive: time.Now(),
			SpeakerID:  speakerID, // <- Store their selected speaker
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

		if messageType == websocket.TextMessage {
			var data map[string]interface{}
			if err := json.Unmarshal(message, &data); err != nil {
				log.Printf("Error parsing message: %v", err)
				continue
			}

			if msgType, ok := data["type"].(string); ok {
				log.Printf("Received message type: %s", msgType)
				
				// Handle different message types
				switch msgType {
				case "audio":
					// For audio data from speaker
					// speakerID := conn.RemoteAddr().String()
					speakerID := c.Query("id")
					if speakerID == "" {
						log.Printf("Missing speaker ID in query")
						conn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Missing speaker ID"}`))
						conn.Close()
						return
					}
					mu.RLock()
					speaker := stream.Speakers[speakerID]
					mu.RUnlock()
					
					if speaker == nil {
						log.Printf("Received audio from unknown speaker: %s", speakerID)
						continue
					}
					
					// Check if audio data is included
					if audioData, ok := data["data"].(string); ok && audioData != "" {
						// Process and forward to Deepgram
						// Note: Client will send base64 encoded audio which needs to be decoded
						// This will be handled in the client-side code
						log.Printf("Received audio data of length %d from speaker %s", len(audioData), speakerID)
					}
				}
			}
		} else if messageType == websocket.BinaryMessage {
			// This is a binary audio message
			// speakerID := conn.RemoteAddr().String()
			speakerID := c.Query("id")
			if speakerID == "" {
				log.Printf("Missing speaker ID in query")
				conn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Missing speaker ID"}`))
				conn.Close()
				return
			}
			mu.RLock()
			speaker := stream.Speakers[speakerID]
			mu.RUnlock()
			
			if speaker == nil {
				log.Printf("Received binary audio from unknown speaker: %s", speakerID)
				continue
			}
			
			// Send the binary audio data directly to Deepgram
			if speaker.DeepgramClient != nil {
				_, err := speaker.DeepgramClient.Write(message)
				if err != nil {
					log.Printf("Error sending audio to Deepgram: %v", err)
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
			voiceId = types.VoiceId("Celine")
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