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

	_ "github.com/lib/pq" // PostgreSQL driver
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
	DB          *sql.DB // Add database connection
}

// Message implements the LiveMessageCallback interface for handling message responses
// func (cb *DeepgramCallback) Message(mr *msginterfaces.MessageResponse) error {
// 	// Skip empty transcripts
// 	sentence := strings.TrimSpace(mr.Channel.Alternatives[0].Transcript)
// 	if len(mr.Channel.Alternatives) == 0 || len(sentence) == 0 {
// 		return nil
// 	}

// 	// Process the transcription
// 	log.Printf("[Deepgram] Transcription: %s (Final: %v)", sentence, mr.IsFinal)

// 	if mr.IsFinal {
// 		// Add to the string builder
// 		cb.sb.WriteString(sentence)
// 		cb.sb.WriteString(" ")

// 		// When speech is final, send the complete transcription
// 		if mr.SpeechFinal {
// 			completedText := cb.sb.String()
// 			log.Printf("[Deepgram] Final speech: %s", completedText)

// 			// Send the transcript to the speaker
// 			speechMsg := map[string]interface{}{
// 				"type":     "transcription",
// 				"text":     completedText,
// 				"language": cb.SourceLang,
// 			}
// 			speechJSON, _ := json.Marshal(speechMsg)
// 			if err := cb.SpeakerConn.WriteMessage(websocket.TextMessage, speechJSON); err != nil {
// 				log.Printf("Failed to send transcription back to speaker: %v", err)
// 			}

// 			// Process translations for audience members
// 			cb.processTranslations(completedText)

// 			// Reset the buffer for the next utterance
// 			cb.sb.Reset()
// 		}
// 	} else {
// 		// For interim results, just log them
// 		log.Printf("[Deepgram] Interim result: %s", sentence)

// 		// Optionally send interim results to the speaker
// 		// This would let them see partial transcriptions as they speak
// 		interimMsg := map[string]interface{}{
// 			"type":     "interim",
// 			"text":     sentence,
// 			"language": cb.SourceLang,
// 		}
// 		interimJSON, _ := json.Marshal(interimMsg)
// 		if err := cb.SpeakerConn.WriteMessage(websocket.TextMessage, interimJSON); err != nil {
// 			log.Printf("Failed to send interim transcription to speaker: %v", err)
// 		}
// 	}
// 	return nil
// }
func (cb *DeepgramCallback) Message(mr *msginterfaces.MessageResponse) error {
	if len(mr.Channel.Alternatives) == 0 {
		return nil
	}
	sentence := strings.TrimSpace(mr.Channel.Alternatives[0].Transcript)
	if sentence == "" {
		return nil
	}

	if mr.IsFinal {
		log.Printf("[Deepgram] Final: %s", sentence)

		// Append finalized fragment to buffer
		cb.sb.WriteString(sentence)
		cb.sb.WriteString(" ")

		text := cb.sb.String()

		// Look for the last sentence-ending punctuation
		splitIdx := strings.LastIndexAny(text, ".!?")
		if splitIdx != -1 {
			complete := strings.TrimSpace(text[:splitIdx+1])
			remaining := strings.TrimSpace(text[splitIdx+1:])

			if complete != "" {
				cb.processTranslations(complete)

				// Optionally send back to speaker as text
				msg := map[string]interface{}{
					"type":     "transcription",
					"text":     complete,
					"language": cb.SourceLang,
				}
				if jsonMsg, err := json.Marshal(msg); err == nil {
					cb.SpeakerConn.WriteMessage(websocket.TextMessage, jsonMsg)
				}
			}

			// Retain the unpunctuated tail for the next chunk
			cb.sb.Reset()
			cb.sb.WriteString(remaining)
		}
	} else {
		log.Printf("[Deepgram] Interim: %s", sentence)
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
// func (cb *DeepgramCallback) UtteranceEnd(ur *msginterfaces.UtteranceEndResponse) error {
// 	utterance := strings.TrimSpace(cb.sb.String())
// 	if len(utterance) > 0 {
// 		log.Printf("[Deepgram] Utterance end: %s", utterance)

// 		// Send the final utterance to the speaker
// 		utteranceMsg := map[string]interface{}{
// 			"type":     "transcription",
// 			"text":     utterance,
// 			"language": cb.SourceLang,
// 			"final":    true,
// 		}
// 		utteranceJSON, _ := json.Marshal(utteranceMsg)
// 		if err := cb.SpeakerConn.WriteMessage(websocket.TextMessage, utteranceJSON); err != nil {
// 			log.Printf("Failed to send utterance to speaker: %v", err)
// 		}

// 		// Process translations for the audience
// 		cb.processTranslations(utterance)

// 		// Reset the buffer for the next utterance
// 		cb.sb.Reset()
// 	} else {
// 		log.Printf("[Deepgram] Empty utterance end received")
// 	}
// 	return nil
// }

func (cb *DeepgramCallback) UtteranceEnd(ur *msginterfaces.UtteranceEndResponse) error {
	// Get any leftover partial sentence that wasn't sent in Message()
	remaining := strings.TrimSpace(cb.sb.String())
	if remaining != "" {
		log.Printf("[Deepgram] Utterance end (flushing leftover): %s", remaining)

		// Send the final leftover fragment to the speaker
		utteranceMsg := map[string]interface{}{
			"type":     "transcription",
			"text":     remaining,
			"language": cb.SourceLang,
			"final":    true,
		}
		utteranceJSON, _ := json.Marshal(utteranceMsg)
		if err := cb.SpeakerConn.WriteMessage(websocket.TextMessage, utteranceJSON); err != nil {
			log.Printf("Failed to send utterance to speaker: %v", err)
		}

		// Translate the leftover partial sentence (final flush)
		cb.processTranslations(remaining)

		// Clear buffer
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

	// Get the current session ID
	var sessionID int
	err := cb.DB.QueryRow(`
		SELECT id FROM speaking_sessions 
		WHERE speaker_id = (SELECT id FROM speakers WHERE google_id = $1)
		AND session_end IS NULL
		ORDER BY session_start DESC LIMIT 1
	`, cb.SpeakerID).Scan(&sessionID)
	if err == nil {
		// Track each unique language that was translated to
		for targetLang := range translations {
			if err := AddTranslatedLanguage(cb.DB, sessionID, targetLang); err != nil {
				log.Printf("Error adding translated language: %v", err)
			}
		}
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
		err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM speakers WHERE google_id = $1)", googleSub).Scan(&exists)
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

// UserInfo represents additional user information
type UserInfo struct {
	ID                int       `json:"id"`
	SpeakerID         int       `json:"speaker_id"`
	PreferredLanguage string    `json:"preferred_language"`
	Timezone          string    `json:"timezone"`
	NotificationPrefs string    `json:"notification_preferences"`
	LastLogin         time.Time `json:"last_login"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// GetUserInfo retrieves user information for a given speaker ID
func GetUserInfo(db *sql.DB, speakerID int) (*UserInfo, error) {
	var userInfo UserInfo
	err := db.QueryRow(`
		SELECT id, speaker_id, preferred_language, timezone, 
			   notification_preferences, last_login, created_at, updated_at
		FROM user_info
		WHERE speaker_id = $1
	`, speakerID).Scan(
		&userInfo.ID,
		&userInfo.SpeakerID,
		&userInfo.PreferredLanguage,
		&userInfo.Timezone,
		&userInfo.NotificationPrefs,
		&userInfo.LastLogin,
		&userInfo.CreatedAt,
		&userInfo.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &userInfo, nil
}

// CreateUserInfo creates a new user information record
func CreateUserInfo(db *sql.DB, userInfo *UserInfo) error {
	_, err := db.Exec(`
		INSERT INTO user_info (
			speaker_id, preferred_language, timezone, 
			notification_preferences, last_login
		) VALUES ($1, $2, $3, $4, $5)
	`,
		userInfo.SpeakerID,
		userInfo.PreferredLanguage,
		userInfo.Timezone,
		userInfo.NotificationPrefs,
		time.Now(),
	)
	return err
}

// UpdateUserInfo updates existing user information
func UpdateUserInfo(db *sql.DB, userInfo *UserInfo) error {
	_, err := db.Exec(`
		UPDATE user_info
		SET preferred_language = $1,
			timezone = $2,
			notification_preferences = $3,
			last_login = $4
		WHERE speaker_id = $5
	`,
		userInfo.PreferredLanguage,
		userInfo.Timezone,
		userInfo.NotificationPrefs,
		time.Now(),
		userInfo.SpeakerID,
	)
	return err
}

// SpeakingSession represents a speaking session record
type SpeakingSession struct {
	ID                  int       `json:"id"`
	SpeakerID           int       `json:"speaker_id"`
	SpeakerName         string    `json:"speaker_name"`
	GoogleSub           string    `json:"google_sub"`
	SessionStart        time.Time `json:"session_start"`
	SessionEnd          time.Time `json:"session_end"`
	TotalDuration       string    `json:"total_duration"`
	TotalAudienceConn   int       `json:"total_audience_connections"`
	LanguagesTranslated []string  `json:"languages_translated"`
	CreatedAt           time.Time `json:"created_at"`
}

// StartSpeakingSession creates a new speaking session record
func StartSpeakingSession(db *sql.DB, speakerID int, speakerName, googleSub string) (int, error) {
	var sessionID int
	log.Printf("Starting new speaking session for speaker ID: %d, name: %s, google_sub: %s", speakerID, speakerName, googleSub)
	err := db.QueryRow(`
		INSERT INTO speaking_sessions 
		(speaker_id, speaker_name, google_sub, session_start)
		VALUES ($1, $2, $3, CURRENT_TIMESTAMP)
		RETURNING id
	`, speakerID, speakerName, googleSub).Scan(&sessionID)
	if err != nil {
		log.Printf("Error creating speaking session: %v", err)
	} else {
		log.Printf("Successfully created speaking session with ID: %d", sessionID)
	}
	return sessionID, err
}

// EndSpeakingSession updates the session end time and calculates duration
func EndSpeakingSession(db *sql.DB, sessionID int) error {
	log.Printf("Attempting to end speaking session with ID: %d", sessionID)
	result, err := db.Exec(`
		UPDATE speaking_sessions 
		SET session_end = CURRENT_TIMESTAMP,
			total_duration = CURRENT_TIMESTAMP - session_start
		WHERE id = $1
	`, sessionID)
	if err != nil {
		log.Printf("Error ending speaking session: %v", err)
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("Error getting rows affected: %v", err)
	} else {
		log.Printf("Successfully ended speaking session. Rows affected: %d", rowsAffected)
	}
	return nil
}

// IncrementAudienceCount increases the audience connection count
func IncrementAudienceCount(db *sql.DB, sessionID int) error {
	log.Printf("Incrementing audience count for session ID: %d", sessionID)

	// First check current value
	var currentCount sql.NullInt32
	err := db.QueryRow("SELECT total_audience_connections FROM speaking_sessions WHERE id = $1", sessionID).Scan(&currentCount)
	if err != nil {
		log.Printf("Error checking current audience count: %v", err)
	} else {
		log.Printf("Current audience count before increment: %v", currentCount)
	}

	result, err := db.Exec(`
		UPDATE speaking_sessions 
		SET total_audience_connections = COALESCE(total_audience_connections, 0) + 1
		WHERE id = $1
	`, sessionID)
	if err != nil {
		log.Printf("Error incrementing audience count: %v", err)
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("Error getting rows affected: %v", err)
	} else {
		log.Printf("Successfully incremented audience count. Rows affected: %d", rowsAffected)
	}

	// Check new value after update
	err = db.QueryRow("SELECT total_audience_connections FROM speaking_sessions WHERE id = $1", sessionID).Scan(&currentCount)
	if err != nil {
		log.Printf("Error checking new audience count: %v", err)
	} else {
		log.Printf("New audience count after increment: %v", currentCount)
	}

	return nil
}

// AddTranslatedLanguage adds a language to the languages_translated array
func AddTranslatedLanguage(db *sql.DB, sessionID int, language string) error {
	log.Printf("Adding translated language %s for session ID: %d", language, sessionID)
	result, err := db.Exec(`
		UPDATE speaking_sessions 
		SET languages_translated = array_append(
			COALESCE(languages_translated, ARRAY[]::text[]),
			$2
		)
		WHERE id = $1
		AND NOT ($2 = ANY(COALESCE(languages_translated, ARRAY[]::text[])))
	`, sessionID, language)
	if err != nil {
		log.Printf("Error adding translated language: %v", err)
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("Error getting rows affected: %v", err)
	} else {
		log.Printf("Successfully added translated language. Rows affected: %d", rowsAffected)
	}
	return nil
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
	// Connect to PostgreSQL database
	dbConnStr := os.Getenv("DATABASE_URL")
	if dbConnStr == "" {
		log.Fatal("DATABASE_URL environment variable is not set")
	}
	db, err := sql.Open("postgres", dbConnStr)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Test the connection
	err = db.Ping()
	if err != nil {
		log.Fatal(err)
	}
	log.Println("Successfully connected to PostgreSQL database")

	createTableSQL := `
    CREATE TABLE IF NOT EXISTS speakers (
        id SERIAL PRIMARY KEY,
        google_id TEXT UNIQUE NOT NULL,
        email TEXT UNIQUE NOT NULL,
        name TEXT,
        speaker_code TEXT UNIQUE NOT NULL,
        created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
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
		err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM speakers WHERE google_id = $1)", userInfo.Sub).Scan(&exists)
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

		// After successful authentication and user creation/update
		var speakerID int
		err = db.QueryRow("SELECT id FROM speakers WHERE google_id = $1", userInfo.Sub).Scan(&speakerID)
		if err != nil {
			log.Printf("Error getting speaker ID: %v", err)
		} else {
			// Check if user info exists
			existingUserInfo, err := GetUserInfo(db, speakerID)
			if err != nil {
				log.Printf("Error checking user info: %v", err)
			} else if existingUserInfo == nil {
				// Create new user info
				newUserInfo := &UserInfo{
					SpeakerID:         speakerID,
					PreferredLanguage: "en-US", // Default language
					Timezone:          "UTC",   // Default timezone
					NotificationPrefs: "{}",    // Default empty preferences
				}
				if err := CreateUserInfo(db, newUserInfo); err != nil {
					log.Printf("Error creating user info: %v", err)
				}
			} else {
				// Update last login
				existingUserInfo.LastLogin = time.Now()
				if err := UpdateUserInfo(db, existingUserInfo); err != nil {
					log.Printf("Error updating user info: %v", err)
				}
			}
		}
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
		VALUES ($1, $2, $3, $4, CURRENT_TIMESTAMP, 'active')
		ON CONFLICT(google_id) DO UPDATE SET
			email = EXCLUDED.email,
			name = EXCLUDED.name,
			updated_at = CURRENT_TIMESTAMP
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
		err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM speakers WHERE google_id = $1)", googleSub).Scan(&exists)
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
		err := db.QueryRow("SELECT name FROM speakers WHERE speaker_code = $1", speakerCode).Scan(&speakerName)
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
		err := db.QueryRow("SELECT speaker_code FROM speakers WHERE google_id = $1", googleSub).Scan(&speakerCode)
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

	// Add new endpoint to get user info
	router.GET("/user-info", authMiddleware(db), func(c *gin.Context) {
		session, _ := store.Get(c.Request, "session-name")
		googleSub, ok := session.Values["google_sub"].(string)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Not authenticated"})
			return
		}

		var speakerID int
		err := db.QueryRow("SELECT id FROM speakers WHERE google_id = $1", googleSub).Scan(&speakerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get speaker ID"})
			return
		}

		userInfo, err := GetUserInfo(db, speakerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get user info"})
			return
		}

		c.JSON(http.StatusOK, userInfo)
	})

	// Add new endpoint to update user info
	router.POST("/user-info", authMiddleware(db), func(c *gin.Context) {
		session, _ := store.Get(c.Request, "session-name")
		googleSub, ok := session.Values["google_sub"].(string)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Not authenticated"})
			return
		}

		var speakerID int
		err := db.QueryRow("SELECT id FROM speakers WHERE google_id = $1", googleSub).Scan(&speakerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get speaker ID"})
			return
		}

		var userInfo UserInfo
		if err := c.BindJSON(&userInfo); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data"})
			return
		}

		userInfo.SpeakerID = speakerID
		if err := UpdateUserInfo(db, &userInfo); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update user info"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "User info updated successfully"})
	})

	// Add new endpoint to get speaker usage statistics
	router.GET("/speaker-usage", authMiddleware(db), func(c *gin.Context) {
		session, _ := store.Get(c.Request, "session-name")
		googleSub, ok := session.Values["google_sub"].(string)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Not authenticated"})
			return
		}

		var stats struct {
			SpeakerName  string  `json:"speaker_name"`
			Email        string  `json:"email"`
			TotalSeconds float64 `json:"total_seconds"`
			TotalHours   float64 `json:"total_hours"`
			SessionCount int     `json:"session_count"`
		}

		err := db.QueryRow(`
			SELECT 
				s.name as speaker_name,
				s.email,
				SUM(EXTRACT(EPOCH FROM (COALESCE(ss.session_end, CURRENT_TIMESTAMP) - ss.session_start))) as total_seconds,
				COUNT(*) as session_count
			FROM speaking_sessions ss
			JOIN speakers s ON ss.speaker_id = s.id
			WHERE s.google_id = $1
			GROUP BY s.name, s.email
		`, googleSub).Scan(&stats.SpeakerName, &stats.Email, &stats.TotalSeconds, &stats.SessionCount)

		if err != nil {
			if err == sql.ErrNoRows {
				c.JSON(http.StatusOK, gin.H{
					"message": "No sessions found",
					"stats": gin.H{
						"total_seconds": 0,
						"total_hours":   0,
						"session_count": 0,
					},
				})
				return
			}
			log.Printf("Error getting speaker usage stats: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get usage statistics"})
			return
		}

		stats.TotalHours = stats.TotalSeconds / 3600

		c.JSON(http.StatusOK, gin.H{
			"speaker_name":  stats.SpeakerName,
			"email":         stats.Email,
			"total_seconds": stats.TotalSeconds,
			"total_hours":   stats.TotalHours,
			"session_count": stats.SessionCount,
		})
	})

	// Add route for usage statistics page
	router.GET("/usage", authMiddleware(db), func(c *gin.Context) {
		c.HTML(http.StatusOK, "usage.html", gin.H{
			"title": "Usage Statistics",
		})
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
	// Declare sessionID at function level
	var sessionID int

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
		err := db.QueryRow("SELECT speaker_code FROM speakers WHERE google_id = $1", googleSub).Scan(&dbSpeakerCode)
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
			err := db.QueryRow("SELECT name FROM speakers WHERE google_id = $1", googleSub).Scan(&speakerName)
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

		// Get the current speaker's session ID
		err = db.QueryRow(`
			SELECT id FROM speaking_sessions 
			WHERE speaker_id = (SELECT id FROM speakers WHERE google_id = $1)
			AND session_end IS NULL
			ORDER BY session_start DESC LIMIT 1
		`, googleSub).Scan(&sessionID)
		if err != nil {
			if err == sql.ErrNoRows {
				// No active session found, create a new one
				log.Printf("No active session found for speaker %s, creating new session", googleSub)

				// Get speaker's database ID and name
				var dbSpeakerID int
				var speakerName string
				err := db.QueryRow("SELECT id, name FROM speakers WHERE google_id = $1", googleSub).Scan(&dbSpeakerID, &speakerName)
				if err != nil {
					log.Printf("Error getting speaker info: %v", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get speaker info"})
					return
				}

				// Start a new speaking session
				sessionID, err = StartSpeakingSession(db, dbSpeakerID, speakerName, googleSub)
				if err != nil {
					log.Printf("Error starting speaking session: %v", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start speaking session"})
					return
				}
				log.Printf("Created new session with ID %d for speaker %s", sessionID, googleSub)
			} else {
				log.Printf("Error getting session ID: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get session ID"})
				return
			}
		} else {
			log.Printf("Using existing session with ID %d for speaker %s", sessionID, googleSub)
		}
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

		// Get speaker's database ID and name
		var dbSpeakerID int
		var speakerName string
		err := db.QueryRow("SELECT id, name FROM speakers WHERE google_id = $1", speakerID).Scan(&dbSpeakerID, &speakerName)
		if err != nil {
			log.Printf("Error getting speaker info: %v", err)
			conn.Close()
			currentStream.mu.Unlock()
			return
		}

		// Start a new speaking session
		//sessionID, err := StartSpeakingSession(db, dbSpeakerID, speakerName, speakerID)

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
			Stream:      currentStream,
			DB:          db, // Add database connection
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
			err := db.QueryRow("SELECT name FROM speakers WHERE google_id = $1", speakerID).Scan(&speakerName)
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
			// End the speaking session when the speaker disconnects
			if err := EndSpeakingSession(db, sessionID); err != nil {
				log.Printf("Error ending speaking session: %v", err)
			}

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
				err := db.QueryRow("SELECT name FROM speakers WHERE google_id = $1", speakerID).Scan(&speakerName)
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

		// Get the current speaker's session ID and increment audience count
		var sessionID int
		err := db.QueryRow(`
			SELECT id FROM speaking_sessions 
			WHERE speaker_id = (SELECT id FROM speakers WHERE speaker_code = $1)
			AND session_end IS NULL
			ORDER BY session_start DESC LIMIT 1
		`, speakerCode).Scan(&sessionID)
		if err == nil {
			log.Printf("Found active session ID %d for speaker code %s, attempting to increment audience count", sessionID, speakerCode)
			if err := IncrementAudienceCount(db, sessionID); err != nil {
				log.Printf("Error incrementing audience count: %v", err)
			}
		} else {
			log.Printf("No active session found for speaker code %s: %v", speakerCode, err)
		}

		// Send list of active speakers to new audience member
		currentStream.mu.RLock()
		activeSpeakersList := make([]string, 0, len(currentStream.Speakers))
		for speakerID := range currentStream.Speakers {
			// Get speaker name from database
			var speakerName string
			err := db.QueryRow("SELECT name FROM speakers WHERE google_id = $1", speakerID).Scan(&speakerName)
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
				log.Printf("Unexpected WebSocket close for %s: %v", role, err)
			} else {
				log.Printf("WebSocket read error for %s: %v", role, err)
			}

			// Handle disconnection based on role
			if role == "speaker" {
				log.Printf("Speaker %s disconnected, ending session", googleSub)
				if err := EndSpeakingSession(db, sessionID); err != nil {
					log.Printf("Error ending speaking session: %v", err)
				}
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
				case "disconnect":
					if role == "speaker" {
						log.Printf("Speaker %s requested disconnect", googleSub)

						// End the speaking session
						if err := EndSpeakingSession(db, sessionID); err != nil {
							log.Printf("Error ending speaking session: %v", err)
						}

						// Send confirmation to client
						disconnectMsg := map[string]interface{}{
							"type":    "disconnected",
							"message": "Session ended successfully",
						}
						disconnectJSON, _ := json.Marshal(disconnectMsg)
						conn.WriteMessage(websocket.TextMessage, disconnectJSON)

						// Clean up the speaker's resources
						currentStream.mu.Lock()
						if speaker, exists := currentStream.Speakers[googleSub]; exists {
							if speaker.DeepgramClient != nil {
								speaker.DeepgramClient.Stop()
							}
							if speaker.DeepgramCancelCtx != nil {
								speaker.DeepgramCancelCtx()
							}
							delete(currentStream.Speakers, googleSub)
						}
						currentStream.mu.Unlock()

						// Close the connection
						conn.Close()
						return
					}
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

// var neuralVoiceSupport = map[string]bool{
// 	"arb":    true, // Arabic
// 	"ar-AE":  true, // Arabic (Gulf)
// 	"ca-ES":  true, // Catalan
// 	"cs-CZ":  true, // Czech
// 	"da-DK":  true, // Danish
// 	"de-DE":  true, // German
// 	"de-AT":  true, // German (Austrian)
// 	"en-US":  true, // English (US)
// 	"en-GB":  true, // English (British)
// 	"en-AU":  true, // English (Australian)
// 	"en-IN":  true, // English (Indian)
// 	"en-NZ":  true, // English (New Zealand)
// 	"en-ZA":  true, // English (South African)
// 	"es-ES":  true, // Spanish (Spain)
// 	"es-MX":  true, // Spanish (Mexican)
// 	"es-US":  true, // Spanish (US)
// 	"fi-FI":  true, // Finnish
// 	"fr-FR":  true, // French (France)
// 	"fr-CA":  true, // French (Canada)
// 	"hi-IN":  true, // Hindi
// 	"is-IS":  true, // Icelandic
// 	"it-IT":  true, // Italian
// 	"ja-JP":  true, // Japanese
// 	"ko-KR":  true, // Korean
// 	"nb-NO":  true, // Norwegian
// 	"nl-NL":  true, // Dutch
// 	"pl-PL":  true, // Polish
// 	"pt-BR":  true, // Portuguese (Brazilian)
// 	"pt-PT":  true, // Portuguese (European)
// 	"ro-RO":  true, // Romanian
// 	"ru-RU":  true, // Russian
// 	"sv-SE":  true, // Swedish
// 	"tr-TR":  true, // Turkish
// 	"zh-CN":  true, // Chinese (Mandarin)
// 	"yue-CN": true,
// 	"fr-BE": true,
// 	"de-CH": true,
// }
var voiceSupportsNeural = map[string]bool{
	"Matthew":   true,
	"Brian":     true,
	"Joanna":    true,
	"Kevin":     true,
	"Kajal":     true,
	"Aria":      true,
	"Zayd":      true,
	"Arlet":     true,
	"Hiujin":    true,
	"Zhiyu":     true,
	"Jitka":     true,
	"Sofie":     true,
	"Laura":     true,
	"Suvi":      true,
	"Rémi":      true,
	"Isabelle":  true,
	"Liam":      true,
	"Daniel":    true,
	"Sabrina":   true,
	"Adriano":   true,
	"Takumi":    true,
	"Seoyeon":   true,
	"Ida":       true,
	"Ola":       true,
	"Thiago":    true,
	"Ines":      true,
	"Sergio":    true,
	"Andrés":    true,
	"Pedro":     true,
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

	var voiceId types.VoiceId
	if req.VoiceId != "" {
		log.Printf("Using provided VoiceId: %s", req.VoiceId)
		voiceId = types.VoiceId(req.VoiceId)
	} else {
		log.Printf("Missing VoiceId; defaulting to 'Matthew'")
		voiceId = types.VoiceId("Matthew")
	}

	log.Printf("Selected voice ID: %s", voiceId)

	// Determine if the language supports neural voices
	// engine := types.EngineStandard
	// if neuralVoiceSupport[req.Language] {
	// 	engine = types.EngineNeural
	// 	log.Printf("Using neural engine for language: %s", req.Language)
	// } else {
	// 	log.Printf("Using standard engine for language: %s (neural not supported)", req.Language)
	// }
	engine := types.EngineStandard
	if voiceSupportsNeural[req.VoiceId] {
		engine = types.EngineNeural
		log.Printf("Using neural engine for voice: %s", req.VoiceId)
	} else {
		log.Printf("Using standard engine for voice: %s (neural not supported)", req.VoiceId)
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
