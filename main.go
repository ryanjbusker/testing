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
	msginterfaces "github.com/deepgram/deepgram-go-sdk/pkg/api/listen/v1/websocket/interfaces"
	clientinterfaces "github.com/deepgram/deepgram-go-sdk/pkg/client/interfaces"
	client "github.com/deepgram/deepgram-go-sdk/pkg/client/listen"
	dgclient "github.com/deepgram/deepgram-go-sdk/pkg/client/listen/v1/websocket"

	"database/sql"

	_ "github.com/mattn/go-sqlite3"
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

// Add a map to track all active streams
var activeStreams = make(map[string]*Stream)
var streamsMu sync.RWMutex // Add mutex for thread-safe access to activeStreams

type GoogleUserInfo struct {
	Sub           string `json:"sub"`            // Google's unique user ID
	Email         string `json:"email"`          // User's email
	Name          string `json:"name"`           // User's full name
	EmailVerified bool   `json:"email_verified"` // Whether email is verified
}
type Stream struct {
	Speakers    map[string]*Speaker
	Audience    map[string]*Audience
	CreatedAt   time.Time
	Name        string
	Description string
	IsActive    bool
	SpeakerCode string
	mu          sync.RWMutex // Add mutex for thread-safe access
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
	sb          *strings.Builder // String builder to accumulate transcription
	Stream      *Stream
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
	cb.Stream.mu.RLock()
	// Group audience by target language
	audienceByLang := make(map[string][]*websocket.Conn)
	for _, audience := range cb.Stream.Audience {
		audienceByLang[audience.Language] = append(audienceByLang[audience.Language], audience.Conn)
	}
	cb.Stream.mu.RUnlock()

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
func authMiddleware(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		log.Printf("Auth middleware: Checking authentication for path: %s", c.Request.URL.Path)

		session, err := store.Get(c.Request, "session-name")
		if err != nil {
			log.Printf("Auth middleware: Session error: %v", err)
			c.Redirect(http.StatusSeeOther, "/login")
			c.Abort()
			return
		}

		googleSub, ok := session.Values["google_sub"].(string)
		if !ok {
			log.Printf("Auth middleware: No google_sub in session")
			c.Redirect(http.StatusSeeOther, "/login")
			c.Abort()
			return
		}

		log.Printf("Auth middleware: Found google_sub: %s", googleSub)

		// Check if user exists in database
		var exists bool
		err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM speakers WHERE google_id = ?)", googleSub).Scan(&exists)
		if err != nil {
			log.Printf("Auth middleware: Database error: %v", err)
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": "Database error occurred",
			})
			c.Abort()
			return
		}

		if !exists {
			log.Printf("Auth middleware: User not found in database")
			c.HTML(http.StatusForbidden, "error.html", gin.H{
				"error": "You are not authorized to access this page",
			})
			c.Abort()
			return
		}

		log.Printf("Auth middleware: Authentication successful")
		c.Next()
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
	///////////////////////////
	db, err := sql.Open("sqlite3", "translation_service.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	createTableSQL := `
    CREATE TABLE IF NOT EXISTS speakers (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        google_id TEXT UNIQUE NOT NULL,
        email TEXT UNIQUE NOT NULL,
        name TEXT,
        speaker_code TEXT UNIQUE NOT NULL,
        created_at DATETIME DEFAULT (datetime('now')),
        payment_status TEXT,
        subscription_id TEXT
    );
    `
	_, err = db.Exec(createTableSQL)
	if err != nil {
		log.Fatalf("Failed to create table: %v", err)
	}

	log.Println("Speakers table created successfully.")
	///////////////////////////
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

	router.GET("/speaker", authMiddleware(db), func(c *gin.Context) {
		log.Printf("Serving speaker.html")
		c.HTML(http.StatusOK, "speaker.html", gin.H{
			"title": "Speaker Page",
		})
	})

	router.GET("/speaker/", authMiddleware(db), func(c *gin.Context) {
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
		handleWebSocket(c, db)
	})

	//The folllowing router.GET was added for OAuth
	//The folllowing router.GET was added for OAuth
	router.GET("/login", func(c *gin.Context) {
		// Get the redirect URL from query parameter
		redirectTo := c.Query("from")
		if redirectTo == "" {
			redirectTo = "/"
		}

		// Store the redirect URL in the session
		session, _ := store.Get(c.Request, "session-name")
		session.Values["redirect_after_login"] = redirectTo
		session.Save(c.Request, c.Writer)

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
		userInfoResp, err := client.Get("https://www.googleapis.com/oauth2/v3/userinfo")
		if err != nil {
			log.Printf("Failed getting user info: %v", err)
			c.String(http.StatusInternalServerError, "Failed getting user info")
			return
		}
		defer userInfoResp.Body.Close()

		// Extract user info including Google's sub ID
		userInfo, err := extractGoogleUserInfo(userInfoResp)
		if err != nil {
			log.Printf("Failed to extract user info: %v", err)
			c.String(http.StatusInternalServerError, "Failed to extract user info")
			return
		}

		log.Printf("=== User Login Details ===")
		log.Printf("Google ID (sub): %s", userInfo.Sub)
		log.Printf("Email: %s", userInfo.Email)
		log.Printf("Name: %s", userInfo.Name)
		log.Printf("========================")

		// Save user info to session
		session, _ := store.Get(c.Request, "session-name")
		session.Values["google_sub"] = userInfo.Sub
		session.Values["email"] = userInfo.Email
		session.Values["name"] = userInfo.Name

		// Set session cookie properties
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

		// Check if user is a speaker
		var exists bool
		err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM speakers WHERE google_id = ?)", userInfo.Sub).Scan(&exists)
		if err != nil {
			log.Printf("Database error checking speaker: %v", err)
			c.String(http.StatusInternalServerError, "Database error occurred")
			return
		}

		// Get the redirect URL from session
		redirectTo, _ := session.Values["redirect_after_login"].(string)
		if redirectTo == "" {
			redirectTo = "/"
		}

		// Clear the redirect URL from session
		delete(session.Values, "redirect_after_login")
		session.Save(c.Request, c.Writer)

		// Redirect to the original destination
		c.Redirect(http.StatusSeeOther, redirectTo)
	})

	router.GET("/logout", func(c *gin.Context) {
		log.Printf("Logout requested")

		// Get the session
		session, err := store.Get(c.Request, "session-name")
		if err != nil {
			log.Printf("Error getting session during logout: %v", err)
		}

		// Log session values before clearing
		log.Printf("Session values before logout: %v", session.Values)

		// Clear all session values
		for k := range session.Values {
			delete(session.Values, k)
		}

		// Set session to expire immediately
		session.Options.MaxAge = -1

		// Save the cleared session
		err = session.Save(c.Request, c.Writer)
		if err != nil {
			log.Printf("Error saving cleared session: %v", err)
		}

		// Explicitly clear the session cookie
		c.SetCookie("session-name", "", -1, "/", "", false, true)

		log.Printf("Logout completed, session cleared")
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
	router.POST("/admin/insert-user", func(c *gin.Context) {
		var user struct {
			GoogleID string `json:"google_id"`
			Email    string `json:"email"`
			Name     string `json:"name"`
		}

		if err := c.BindJSON(&user); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data"})
			return
		}

		insertUserSQL := `
		INSERT INTO speakers (google_id, email, name, speaker_code, created_at, payment_status)
		VALUES (?, ?, ?, ?, datetime('now'), 'active')
		ON CONFLICT(google_id) DO UPDATE SET
			email = excluded.email,
			name = excluded.name,
			updated_at = datetime('now')
		`
		_, err := db.Exec(insertUserSQL, user.GoogleID, user.Email, user.Name, user.GoogleID)
		if err != nil {
			log.Printf("Failed to insert/update user in database: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save user data"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "User inserted successfully"})
	})

	// Modify the check-speaker endpoint to verify against database
	router.GET("/check-speaker", func(c *gin.Context) {
		session, _ := store.Get(c.Request, "session-name")
		googleSub, ok := session.Values["google_sub"].(string)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Not logged in"})
			return
		}

		// Check if user exists in database
		var exists bool
		err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM speakers WHERE google_id = ?)", googleSub).Scan(&exists)
		if err != nil {
			log.Printf("Database error checking speaker: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error occurred"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"is_speaker": exists,
			"user_info": gin.H{
				"sub":   googleSub,
				"email": session.Values["email"],
				"name":  session.Values["name"],
			},
		})
	})

	// Add a new endpoint for requesting speaker access
	router.GET("/request-access", func(c *gin.Context) {
		session, _ := store.Get(c.Request, "session-name")
		googleSub, ok := session.Values["google_sub"].(string)
		if !ok {
			c.Redirect(http.StatusSeeOther, "/login")
			return
		}

		c.HTML(http.StatusOK, "request-access.html", gin.H{
			"user_info": gin.H{
				"sub":   googleSub,
				"email": session.Values["email"],
				"name":  session.Values["name"],
			},
		})
	})

	router.GET("/audience/:speakerCode", func(c *gin.Context) {
		speakerCode := c.Param("speakerCode")

		// Check if speaker exists in database
		var speakerName string
		err := db.QueryRow("SELECT name FROM speakers WHERE speaker_code = ?", speakerCode).Scan(&speakerName)
		if err != nil {
			if err == sql.ErrNoRows {
				c.HTML(http.StatusNotFound, "error.html", gin.H{
					"error": "Speaker not found",
				})
				return
			}
			log.Printf("Database error: %v", err)
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": "Internal server error",
			})
			return
		}

		// Check if stream exists, if not create it
		streamsMu.Lock()
		stream, exists := activeStreams[speakerCode]
		if !exists {
			stream = &Stream{
				Name:        speakerName + "'s Stream",
				Description: "Live translation stream",
				Speakers:    make(map[string]*Speaker),
				Audience:    make(map[string]*Audience),
				IsActive:    true,
				CreatedAt:   time.Now(),
				SpeakerCode: speakerCode,
			}
			activeStreams[speakerCode] = stream
		}
		streamsMu.Unlock()

		c.HTML(http.StatusOK, "audience.html", gin.H{
			"title":       "Audience Page",
			"speakerName": speakerName,
			"speakerCode": speakerCode,
		})
	})

	router.GET("/get-speaker-code", authMiddleware(db), func(c *gin.Context) {
		session, _ := store.Get(c.Request, "session-name")
		googleSub, ok := session.Values["google_sub"].(string)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Not authenticated"})
			return
		}

		var speakerCode string
		err := db.QueryRow("SELECT speaker_code FROM speakers WHERE google_id = ?", googleSub).Scan(&speakerCode)
		if err != nil {
			if err == sql.ErrNoRows {
				c.JSON(http.StatusNotFound, gin.H{"error": "Speaker not found"})
				return
			}
			log.Printf("Database error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"speaker_code": speakerCode})
	})

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

func handleWebSocket(c *gin.Context, db *sql.DB) {
	role := c.Query("role")
	lang := c.Query("lang")
	speakerCode := c.Query("speaker_code")
	var googleSub string

	log.Printf("WebSocket connection request - Role: %s, SpeakerCode: %s", role, speakerCode)

	// Get the appropriate stream
	var currentStream *Stream
	if role == "speaker" {
		// For speakers, we need to verify their speaker code
		session, _ := store.Get(c.Request, "session-name")
		var ok bool
		googleSub, ok = session.Values["google_sub"].(string)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Not authenticated"})
			return
		}

		// Verify the speaker code matches the authenticated user
		var dbSpeakerCode string
		err := db.QueryRow("SELECT speaker_code FROM speakers WHERE google_id = ?", googleSub).Scan(&dbSpeakerCode)
		if err != nil {
			log.Printf("Database error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
			return
		}

		if dbSpeakerCode != speakerCode {
			c.JSON(http.StatusForbidden, gin.H{"error": "Invalid speaker code"})
			return
		}

		streamsMu.Lock()
		stream, exists := activeStreams[speakerCode]
		if !exists {
			// Get speaker name for stream
			var speakerName string
			err := db.QueryRow("SELECT name FROM speakers WHERE google_id = ?", googleSub).Scan(&speakerName)
			if err != nil {
				log.Printf("Database error getting speaker name: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
				streamsMu.Unlock()
				return
			}

			stream = &Stream{
				Name:        speakerName + "'s Stream",
				Description: "Live translation stream",
				Speakers:    make(map[string]*Speaker),
				Audience:    make(map[string]*Audience),
				IsActive:    true,
				CreatedAt:   time.Now(),
				SpeakerCode: speakerCode,
			}
			activeStreams[speakerCode] = stream
		}
		currentStream = stream
		streamsMu.Unlock()
	} else if role == "audience" {
		// For audience members, we need to find the stream by speaker code
		if speakerCode == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Missing speaker code"})
			return
		}

		streamsMu.RLock()
		stream, exists := activeStreams[speakerCode]
		streamsMu.RUnlock()

		if !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "Stream not found"})
			return
		}
		currentStream = stream
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid role"})
		return
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
		"streamName": currentStream.Name,
		"streamDesc": currentStream.Description,
	}
	successJSON, _ := json.Marshal(successMsg)
	if err := conn.WriteMessage(websocket.TextMessage, successJSON); err != nil {
		log.Printf("Failed to send connection success message: %v", err)
		conn.Close()
		return
	}

	if role == "speaker" {
		speakerID := googleSub // Use Google ID as speaker ID
		currentStream.mu.Lock()
		_, exists := currentStream.Speakers[speakerID]
		if exists {
			log.Printf("Speaker ID %s is already in use", speakerID)
			conn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Speaker ID already in use"}`))
			conn.Close()
			currentStream.mu.Unlock()
			return
		}

		// Create context for Deepgram client
		ctx, cancel := context.WithCancel(context.Background())

		// Set up Deepgram transcription options
		transcriptionOptions := &clientinterfaces.LiveTranscriptionOptions{
			Language:       lang,
			Model:          "nova-2",
			Punctuate:      true,
			Encoding:       "linear16",
			SampleRate:     16000,
			Channels:       1,
			SmartFormat:    true,
			InterimResults: true,
			UtteranceEndMs: "1000",
			VadEvents:      true,
		}

		callback := &DeepgramCallback{
			SourceLang:  lang,
			SpeakerID:   speakerID,
			SpeakerConn: conn,
			sb:          &strings.Builder{},
			Stream:      currentStream, // Pass the stream to the callback
		}

		clientOptions := &clientinterfaces.ClientOptions{
			EnableKeepAlive: true,
		}

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
			currentStream.mu.Unlock()
			return
		}

		connected := deepgramClient.Connect()
		if !connected {
			log.Printf("Failed to connect to Deepgram WebSocket API")
			cancel()
			conn.Close()
			currentStream.mu.Unlock()
			return
		}

		speaker := &Speaker{
			Conn:              conn,
			Language:          lang,
			LastActive:        time.Now(),
			DeepgramClient:    deepgramClient,
			DeepgramCtx:       ctx,
			DeepgramCancelCtx: cancel,
		}

		currentStream.Speakers[speakerID] = speaker
		currentStream.mu.Unlock()

		// Notify all audience members about new speaker
		currentStream.mu.RLock()
		for _, audience := range currentStream.Audience {
			// Get speaker name from database
			var speakerName string
			err := db.QueryRow("SELECT name FROM speakers WHERE google_id = ?", speakerID).Scan(&speakerName)
			if err != nil {
				log.Printf("Error getting speaker name: %v", err)
				speakerName = "Unknown Speaker"
			}
			speakerJoinedMsg := map[string]interface{}{
				"type":    "speaker_joined",
				"speaker": speakerName,
			}
			speakerJoinedJSON, _ := json.Marshal(speakerJoinedMsg)
			audience.Conn.WriteMessage(websocket.TextMessage, speakerJoinedJSON)
		}
		currentStream.mu.RUnlock()

		defer func() {
			deepgramClient.Stop()
			cancel()
			conn.Close()

			currentStream.mu.Lock()
			delete(currentStream.Speakers, speakerID)
			if len(currentStream.Speakers) == 0 && len(currentStream.Audience) == 0 {
				streamsMu.Lock()
				delete(activeStreams, speakerCode)
				streamsMu.Unlock()
				log.Printf("Stream %s is now inactive", speakerCode)
			}
			currentStream.mu.Unlock()

			// Notify audience members
			currentStream.mu.RLock()
			for _, audience := range currentStream.Audience {
				// Get speaker name from database
				var speakerName string
				err := db.QueryRow("SELECT name FROM speakers WHERE google_id = ?", speakerID).Scan(&speakerName)
				if err != nil {
					log.Printf("Error getting speaker name: %v", err)
					speakerName = "Unknown Speaker"
				}
				leaveMsg := map[string]interface{}{
					"type":    "speaker_left",
					"speaker": speakerName,
				}
				leaveJSON, _ := json.Marshal(leaveMsg)
				audience.Conn.WriteMessage(websocket.TextMessage, leaveJSON)
			}
			currentStream.mu.RUnlock()
		}()
	} else if role == "audience" {
		audienceID := conn.RemoteAddr().String()
		audience := &Audience{
			Conn:       conn,
			Language:   lang,
			LastActive: time.Now(),
			SpeakerID:  speakerCode,
		}

		currentStream.mu.Lock()
		currentStream.Audience[audienceID] = audience
		currentStream.mu.Unlock()

		// Send list of active speakers to new audience member
		currentStream.mu.RLock()
		activeSpeakersList := make([]string, 0, len(currentStream.Speakers))
		for speakerID := range currentStream.Speakers {
			// Get speaker name from database
			var speakerName string
			err := db.QueryRow("SELECT name FROM speakers WHERE google_id = ?", speakerID).Scan(&speakerName)
			if err != nil {
				log.Printf("Error getting speaker name: %v", err)
				speakerName = "Unknown Speaker"
			}
			activeSpeakersList = append(activeSpeakersList, speakerName)
		}
		currentStream.mu.RUnlock()

		if len(activeSpeakersList) > 0 {
			speakersMsg := map[string]interface{}{
				"type":     "active_speakers",
				"speakers": activeSpeakersList,
			}
			speakersJSON, _ := json.Marshal(speakersMsg)
			conn.WriteMessage(websocket.TextMessage, speakersJSON)
		}

		defer func() {
			currentStream.mu.Lock()
			conn.Close()
			delete(currentStream.Audience, audienceID)
			if len(currentStream.Speakers) == 0 && len(currentStream.Audience) == 0 {
				streamsMu.Lock()
				delete(activeStreams, speakerCode)
				streamsMu.Unlock()
				log.Printf("Stream %s is now inactive", speakerCode)
			}
			currentStream.mu.Unlock()
		}()
	}

	// Handle incoming messages
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Error reading message: %v", err)
			}
			break
		}

		if messageType == websocket.TextMessage {
			var data map[string]interface{}
			if err := json.Unmarshal(message, &data); err != nil {
				log.Printf("Error parsing message: %v", err)
				continue
			}

			if msgType, ok := data["type"].(string); ok {
				log.Printf("Received message type: %s", msgType)

				switch msgType {
				case "audio":
					if role == "speaker" {
						speakerID := googleSub
						currentStream.mu.RLock()
						speaker := currentStream.Speakers[speakerID]
						currentStream.mu.RUnlock()

						if speaker == nil {
							log.Printf("Received audio from unknown speaker: %s", speakerID)
							continue
						}

						if audioData, ok := data["data"].(string); ok && audioData != "" {
							log.Printf("Received audio data of length %d from speaker %s", len(audioData), speakerID)
						}
					}
				}
			}
		} else if messageType == websocket.BinaryMessage {
			if role == "speaker" {
				speakerID := googleSub
				currentStream.mu.RLock()
				speaker := currentStream.Speakers[speakerID]
				currentStream.mu.RUnlock()

				if speaker == nil {
					log.Printf("Received binary audio from unknown speaker: %s", speakerID)
					continue
				}

				if speaker.DeepgramClient != nil {
					_, err := speaker.DeepgramClient.Write(message)
					if err != nil {
						log.Printf("Error sending audio to Deepgram: %v", err)
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

func extractGoogleUserInfo(resp *http.Response) (*GoogleUserInfo, error) {
	var userInfo GoogleUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
		log.Printf("Error decoding user info: %v", err)
		return nil, err
	}
	return &userInfo, nil
}

var neuralVoiceSupport = map[string]bool{
	"en-US": true, // English (US)
	"en-GB": true, // English (British)
	"en-AU": true, // English (Australian)
	"en-NZ": true, // English (New Zealand)
	"en-IN": true, // English (Indian)
	"es-ES": true, // Spanish (European)
	"es-MX": true, // Spanish (Mexican)
	"fr-FR": true, // French
	"de-DE": true, // German
	"it-IT": true, // Italian
	"pt-BR": true, // Portuguese (Brazilian)
	"ja-JP": true, // Japanese
	"ko-KR": true, // Korean
	"zh-CN": true, // Chinese (Mandarin)
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

	voiceId := types.VoiceId("Matthew")
	if req.VoiceId != "" {
		voiceId = types.VoiceId(req.VoiceId)
	} else {
		// Otherwise, select voice based on language
		switch req.Language {
		case "en-US":
			voiceId = types.VoiceId("Matthew")
		case "en-GB":
			voiceId = types.VoiceId("Brian")
		case "en-AU":
			voiceId = types.VoiceId("Olivia")
		case "en-IN":
			voiceId = types.VoiceId("Kajal")
		case "es-ES":
			voiceId = types.VoiceId("Sergio")
		case "es-MX":
			voiceId = types.VoiceId("Mia")
		case "fr-FR":
			voiceId = types.VoiceId("Celine")
		case "de-DE":
			voiceId = types.VoiceId("Vicki")
		case "it-IT":
			voiceId = types.VoiceId("Carla")
		case "pt-BR":
			voiceId = types.VoiceId("Camila")
		case "ja-JP":
			voiceId = types.VoiceId("Takumi")
		case "ko-KR":
			voiceId = types.VoiceId("Seoyeon")
		case "zh-CN":
			voiceId = types.VoiceId("Zhiyu")
		}
	}

	log.Printf("Selected voice ID: %s", voiceId)

	// Determine if the language supports neural voices
	engine := types.EngineStandard
	if neuralVoiceSupport[req.Language] {
		engine = types.EngineNeural
		log.Printf("Using neural engine for language: %s", req.Language)
	} else {
		log.Printf("Using standard engine for language: %s (neural not supported)", req.Language)
	}

	input := &polly.SynthesizeSpeechInput{
		Text:         aws.String(req.Text),
		OutputFormat: types.OutputFormatMp3,
		VoiceId:      voiceId,
		Engine:       engine,
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
