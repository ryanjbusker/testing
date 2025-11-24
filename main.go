package main

import (
	"context" //Added for OAuth
	"encoding/json"
	"fmt"
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

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/polly"

	// Add Deepgram SDK imports
	msginterfaces "github.com/deepgram/deepgram-go-sdk/pkg/api/listen/v1/websocket/interfaces"
	clientinterfaces "github.com/deepgram/deepgram-go-sdk/pkg/client/interfaces"
	client "github.com/deepgram/deepgram-go-sdk/pkg/client/listen"
	dgclient "github.com/deepgram/deepgram-go-sdk/pkg/client/listen/v1/websocket"

	"database/sql"

	_ "github.com/lib/pq" // PostgreSQL driver

	"github.com/stripe/stripe-go/v78"
	"github.com/stripe/stripe-go/v78/subscription"
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
	ID                int
	GoogleID          string
	Email             string
	Name              string
	SpeakerCode       string
	CreatedAt         time.Time
	PaymentStatus     string
	SubscriptionID    string
	StripeCustomerID  string
	PlanName          string
	Conn              *websocket.Conn
	Language          string
	LastActive        time.Time
	DeepgramClient    *dgclient.WSCallback
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

	// Initialize Stripe
	stripe.Key = os.Getenv("STRIPE_SECRET_KEY")

	// Log Stripe configuration status
	log.Printf("Stripe Secret Key configured: %v", stripe.Key != "")
	log.Printf("Stripe Publishable Key configured: %v", os.Getenv("STRIPE_PUBLISHABLE_KEY") != "")
	log.Printf("Stripe Monthly 5h Price ID configured: %v", os.Getenv("STRIPE_MONTHLY_5_PRICE_ID") != "")
	log.Printf("Stripe Monthly 8h Price ID configured: %v", os.Getenv("STRIPE_MONTHLY_8_PRICE_ID") != "")
	log.Printf("Stripe Monthly 13h Price ID configured: %v", os.Getenv("STRIPE_MONTHLY_13_PRICE_ID") != "")
	log.Printf("Stripe Yearly 50h Price ID configured: %v", os.Getenv("STRIPE_YEARLY_50_PRICE_ID") != "")
	log.Printf("Stripe Yearly 100h Price ID configured: %v", os.Getenv("STRIPE_YEARLY_100_PRICE_ID") != "")
	log.Printf("Stripe Yearly 150h Price ID configured: %v", os.Getenv("STRIPE_YEARLY_150_PRICE_ID") != "")
	// Log voice add-on price ID configuration
	log.Printf("Stripe Voice ElevenLabs 8H Price ID configured: %v", os.Getenv("STRIPE_VOICE_ELEVENLABS_8H_PRICE_ID") != "")
	log.Printf("Stripe Voice ElevenLabs 8H Yearly Price ID configured: %v", os.Getenv("STRIPE_VOICE_ELEVENLABS_8H_YEARLY_PRICE_ID") != "")
	log.Printf("Stripe Voice ElevenLabs 50H Price ID configured: %v", os.Getenv("STRIPE_VOICE_ELEVENLABS_50H_PRICE_ID") != "")
	log.Printf("Stripe Voice ElevenLabs 100H Price ID configured: %v", os.Getenv("STRIPE_VOICE_ELEVENLABS_100H_PRICE_ID") != "")
	log.Printf("Stripe Voice ElevenLabs 150H Price ID configured: %v", os.Getenv("STRIPE_VOICE_ELEVENLABS_150H_PRICE_ID") != "")

	log.Printf("Stripe Voice Custom 8H Price ID configured: %v", os.Getenv("STRIPE_VOICE_CUSTOM_8H_PRICE_ID") != "")
	log.Printf("Stripe Voice Custom 8H Yearly Price ID configured: %v", os.Getenv("STRIPE_VOICE_CUSTOM_8H_YEARLY_PRICE_ID") != "")
	log.Printf("Stripe Voice Custom 50H Price ID configured: %v", os.Getenv("STRIPE_VOICE_CUSTOM_50H_PRICE_ID") != "")
	log.Printf("Stripe Voice Custom 100H Price ID configured: %v", os.Getenv("STRIPE_VOICE_CUSTOM_100H_PRICE_ID") != "")
	log.Printf("Stripe Voice Custom 150H Price ID configured: %v", os.Getenv("STRIPE_VOICE_CUSTOM_150H_PRICE_ID") != "")

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
	// Dynamic cookie domain based on environment
	cookieDomain := os.Getenv("COOKIE_DOMAIN")
	if cookieDomain == "" {
		// Default to no domain restriction (works for both localhost and render)
		cookieDomain = ""
	}

	store.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   86400 * 7, // 7 days
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode, // or SameSiteStrictMode
		Secure:   os.Getenv("ENV") == "production",
		Domain:   cookieDomain, // Will be empty for localhost/render, set for custom domain
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

	// Check for any existing unclosed sessions for this speaker
	var existingSessionID int
	err := db.QueryRow(`
		SELECT id FROM speaking_sessions 
		WHERE speaker_id = $1 AND session_end IS NULL
		ORDER BY session_start DESC LIMIT 1
	`, speakerID).Scan(&existingSessionID)

	if err == nil {
		log.Printf("Found existing unclosed session ID %d for speaker %d, closing it first", existingSessionID, speakerID)
		if err := EndSpeakingSession(db, existingSessionID); err != nil {
			log.Printf("Warning: Failed to close existing session %d: %v", existingSessionID, err)
		}
	} else if err != sql.ErrNoRows {
		log.Printf("Error checking for existing sessions: %v", err)
	}

	err = db.QueryRow(`
		INSERT INTO speaking_sessions 
		(speaker_id, speaker_name, google_sub, session_start)
		VALUES ($1, $2, $3, CURRENT_TIMESTAMP)
		RETURNING id
	`, speakerID, speakerName, googleSub).Scan(&sessionID)
	if err != nil {
		log.Printf("Error creating speaking session: %v", err)
	} else {
		log.Printf("Successfully created speaking session with ID: %d for billing tracking", sessionID)
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

// ReportUsageToStripe reports the usage to Stripe based on speaking sessions
func ReportUsageToStripe(db *sql.DB, speakerID string) error {
	paymentService := NewPaymentService(db)
	return paymentService.ReportUsageToStripe(speakerID)
}

// Add function to create metered subscription
func createMeteredSubscription(db *sql.DB, speaker *Speaker) error {
	paymentService := NewPaymentService(db)
	return paymentService.CreateMeteredSubscription(speaker)
}

// Add a function to update speaker's subscription
func updateSpeakerSubscription(db *sql.DB, speakerID string, subscriptionID string) error {
	_, err := db.Exec("UPDATE speakers SET subscription_id = $1 WHERE google_id = $2", subscriptionID, speakerID)
	return err
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
        subscription_id TEXT,
        stripe_customer_id TEXT,
        plan_name TEXT,
        voice_preference TEXT DEFAULT 'polly'
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

	router.GET("/streams", authMiddleware(db), func(c *gin.Context) {
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

		// Note: User info tracking removed - not needed for current functionality
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

	router.GET("/account", authMiddleware(db), func(c *gin.Context) {
		log.Printf("Serving account.html")
		c.HTML(http.StatusOK, "account.html", gin.H{
			"title": "Account",
		})
	})

	// Add account details endpoint
	router.GET("/account-details", authMiddleware(db), func(c *gin.Context) {
		session, _ := store.Get(c.Request, "session-name")
		googleSub, ok := session.Values["google_sub"].(string)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Not authenticated"})
			return
		}

		var accountDetails struct {
			PaymentStatus    string         `json:"payment_status"`
			SubscriptionID   string         `json:"subscription_id"`
			StripeCustomerID string         `json:"stripe_customer_id"`
			PlanName         string         `json:"plan_name"`
			VoiceID          sql.NullString `json:"voice_id"`
			VoicePreference  string         `json:"voice_preference"`
			CreatedAt        time.Time      `json:"created_at"`
		}

		err := db.QueryRow(`
			SELECT payment_status, subscription_id, stripe_customer_id, plan_name, voice_id, voice_preference, created_at
			FROM speakers 
			WHERE google_id = $1
		`, googleSub).Scan(&accountDetails.PaymentStatus, &accountDetails.SubscriptionID, &accountDetails.StripeCustomerID, &accountDetails.PlanName, &accountDetails.VoiceID, &accountDetails.VoicePreference, &accountDetails.CreatedAt)

		// Debug logging
		log.Printf("Account details for user %s: payment_status=%s, plan_name=%s, voice_preference=%s",
			googleSub, accountDetails.PaymentStatus, accountDetails.PlanName, accountDetails.VoicePreference)

		if err != nil {
			if err == sql.ErrNoRows {
				c.JSON(http.StatusNotFound, gin.H{"error": "Speaker not found"})
				return
			}
			log.Printf("Database error: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
			return
		}

		// Determine subscription plan based on stored plan name and payment status
		var subscriptionPlan string
		var nextBilling string
		var planDetails string
		var amountDue string
		var billingCycle string

		// Debug logging for condition check
		log.Printf("Condition check: payment_status='%s', plan_name='%s', condition result=%v",
			accountDetails.PaymentStatus, accountDetails.PlanName,
			accountDetails.PaymentStatus == "active" && accountDetails.PlanName != "")

		if accountDetails.PaymentStatus == "active" && accountDetails.PlanName != "" {
			// Use the stored plan name to determine the display name and details
			switch accountDetails.PlanName {
			case "monthly-8":
				subscriptionPlan = "Starter Monthly"
				planDetails = "8 hours/month"
				amountDue = "$400"
				billingCycle = "Monthly"
			case "yearly-8":
				subscriptionPlan = "Just in Case"
				planDetails = "8 hours/year"
				amountDue = "$400"
				billingCycle = "Yearly"
			case "yearly-50":
				subscriptionPlan = "Starter Yearly"
				planDetails = "50 hours/year"
				amountDue = "$2,500"
				billingCycle = "Yearly"
			case "yearly-100":
				subscriptionPlan = "Professional Yearly"
				planDetails = "100 hours/year"
				amountDue = "$4,000"
				billingCycle = "Yearly"
			case "yearly-150":
				subscriptionPlan = "Enterprise Yearly"
				planDetails = "150 hours/year"
				amountDue = "$4,500"
				billingCycle = "Yearly"
			default:
				subscriptionPlan = "Active Subscription"
				planDetails = "Custom Plan"
				amountDue = "Contact Support"
				billingCycle = "Custom"
			}

			// Fetch next billing date and amount from Stripe if subscription ID exists
			if accountDetails.SubscriptionID != "" {
				log.Printf("Attempting to fetch Stripe subscription: %s", accountDetails.SubscriptionID)
				stripe.Key = os.Getenv("STRIPE_SECRET_KEY")
				sub, err := subscription.Get(accountDetails.SubscriptionID, nil)
				if err != nil {
					log.Printf("Error fetching Stripe subscription: %v", err)
					nextBilling = "Unable to fetch billing date"
				} else {
					log.Printf("Successfully fetched Stripe subscription, current period end: %d", sub.CurrentPeriodEnd)
					// Convert Unix timestamp to readable date
					nextBillingTime := time.Unix(sub.CurrentPeriodEnd, 0)
					nextBilling = nextBillingTime.Format("January 2, 2006")

					// Get the actual amount due from Stripe
					if sub.Items != nil && len(sub.Items.Data) > 0 {
						totalAmount := int64(0)
						for _, item := range sub.Items.Data {
							if item.Price != nil {
								totalAmount += item.Price.UnitAmount
							}
						}
						amountDue = fmt.Sprintf("$%.2f", float64(totalAmount)/100)
					}
				}
			} else {
				nextBilling = "Next billing cycle"
			}
		} else if accountDetails.PaymentStatus == "active" {
			subscriptionPlan = "Free Plan"
			planDetails = "Limited access"
			nextBilling = "N/A"
			amountDue = "$0"
			billingCycle = "N/A"
		} else {
			subscriptionPlan = "Inactive"
			planDetails = "No active subscription"
			nextBilling = "N/A"
			amountDue = "$0"
			billingCycle = "N/A"
		}

		// Handle nullable voice_id
		var voiceID string
		if accountDetails.VoiceID.Valid {
			voiceID = accountDetails.VoiceID.String
		}

		c.JSON(http.StatusOK, gin.H{
			"payment_status":     accountDetails.PaymentStatus,
			"subscription_id":    accountDetails.SubscriptionID,
			"stripe_customer_id": accountDetails.StripeCustomerID,
			"created_at":         accountDetails.CreatedAt.Format("2006-01-02 15:04:05"),
			"subscription_plan":  subscriptionPlan,
			"plan_details":       planDetails,
			"next_billing":       nextBilling,
			"amount_due":         amountDue,
			"billing_cycle":      billingCycle,
			"voice_preference":   accountDetails.VoicePreference,
			"voice_id":           voiceID,
			"last_login":         time.Now().Format("2006-01-02 15:04:05"), // You can enhance this with actual last login tracking
		})
	})

	router.GET("/session", func(c *gin.Context) {
		session, _ := store.Get(c.Request, "session-name")
		email := session.Values["email"]
		name := session.Values["name"]
		c.JSON(http.StatusOK, gin.H{"email": email, "name": name})
	})

	// Add Polly TTS endpoint
	router.POST("/polly-tts", handlePollyTTS(db))
	// Add demo TTS endpoint for voice cloning demos
	router.POST("/demo-tts", handleDemoTTS(db))
	// Add ElevenLabs voice creation endpoint
	router.POST("/api/create-elevenlabs-voice", handleCreateElevenLabsVoice(db))
	// Add endpoint to get available voices based on subscription
	router.GET("/api/available-voices", authMiddleware(db), handleGetAvailableVoices(db))

	// Add delete account endpoint
	router.POST("/delete-account", authMiddleware(db), handleDeleteAccount(db))

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

	// Add new endpoint to get speaker usage statistics
	router.GET("/speaker-usage", authMiddleware(db), func(c *gin.Context) {
		session, _ := store.Get(c.Request, "session-name")
		googleSub, ok := session.Values["google_sub"].(string)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Not authenticated"})
			return
		}

		var stats struct {
			SpeakerName          string  `json:"speaker_name"`
			Email                string  `json:"email"`
			TotalSeconds         float64 `json:"total_seconds"`
			TotalHours           float64 `json:"total_hours"`
			SessionCount         int     `json:"session_count"`
			CurrentMonthSeconds  float64 `json:"current_month_seconds"`
			CurrentMonthHours    float64 `json:"current_month_hours"`
			CurrentMonthSessions int     `json:"current_month_sessions"`
			PlanName             string  `json:"plan_name"`
			SubscriptionStatus   string  `json:"subscription_status"`
		}

		// Get overall stats
		err := db.QueryRow(`
			SELECT 
				s.name as speaker_name,
				s.email,
				s.plan_name,
				s.payment_status,
				SUM(EXTRACT(EPOCH FROM (COALESCE(ss.session_end, CURRENT_TIMESTAMP) - ss.session_start))) as total_seconds,
				COUNT(*) as session_count
			FROM speaking_sessions ss
			JOIN speakers s ON ss.speaker_id = s.id
			WHERE s.google_id = $1
			GROUP BY s.name, s.email, s.plan_name, s.payment_status
		`, googleSub).Scan(&stats.SpeakerName, &stats.Email, &stats.PlanName, &stats.SubscriptionStatus, &stats.TotalSeconds, &stats.SessionCount)

		if err != nil {
			if err == sql.ErrNoRows {
				c.JSON(http.StatusOK, gin.H{
					"message": "No sessions found",
					"stats": gin.H{
						"total_seconds":          0,
						"total_hours":            0,
						"session_count":          0,
						"current_month_seconds":  0,
						"current_month_hours":    0,
						"current_month_sessions": 0,
						"plan_name":              "",
						"subscription_status":    "",
					},
				})
				return
			}
			log.Printf("Error getting speaker usage stats: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get usage statistics"})
			return
		}

		// Get current month stats
		err = db.QueryRow(`
			SELECT 
				SUM(EXTRACT(EPOCH FROM (COALESCE(ss.session_end, CURRENT_TIMESTAMP) - ss.session_start))) as current_month_seconds,
				COUNT(*) as current_month_sessions
			FROM speaking_sessions ss
			JOIN speakers s ON ss.speaker_id = s.id
			WHERE s.google_id = $1
			AND ss.session_start >= DATE_TRUNC('month', CURRENT_DATE)
		`, googleSub).Scan(&stats.CurrentMonthSeconds, &stats.CurrentMonthSessions)

		if err != nil && err != sql.ErrNoRows {
			log.Printf("Error getting current month stats: %v", err)
		}

		stats.TotalHours = stats.TotalSeconds / 3600
		stats.CurrentMonthHours = stats.CurrentMonthSeconds / 3600

		c.JSON(http.StatusOK, gin.H{
			"speaker_name":           stats.SpeakerName,
			"email":                  stats.Email,
			"total_seconds":          stats.TotalSeconds,
			"total_hours":            stats.TotalHours,
			"session_count":          stats.SessionCount,
			"current_month_seconds":  stats.CurrentMonthSeconds,
			"current_month_hours":    stats.CurrentMonthHours,
			"current_month_sessions": stats.CurrentMonthSessions,
			"plan_name":              stats.PlanName,
			"subscription_status":    stats.SubscriptionStatus,
		})
	})

	// Add route for join page - requires Google OAuth but not necessarily speaker status
	router.GET("/join", func(c *gin.Context) {
		// Check if user is authenticated with Google OAuth
		session, err := store.Get(c.Request, "session-name")
		if err != nil {
			c.Redirect(http.StatusSeeOther, "/login?from=/join")
			return
		}

		googleSub, ok := session.Values["google_sub"].(string)
		if !ok {
			c.Redirect(http.StatusSeeOther, "/login?from=/join")
			return
		}

		// Check if user is already a speaker
		var exists bool
		err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM speakers WHERE google_id = $1)", googleSub).Scan(&exists)
		if err != nil {
			log.Printf("Error checking speaker status: %v", err)
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"error": "Database error occurred",
			})
			return
		}

		if exists {
			// User is already a speaker, redirect to speaker page
			c.Redirect(http.StatusSeeOther, "/speaker")
			return
		}

		// User is authenticated but not a speaker - show join page
		c.HTML(http.StatusOK, "join.html", gin.H{
			"title":                "Join BMM Translation",
			"StripePublishableKey": os.Getenv("STRIPE_PUBLISHABLE_KEY"),
		})
	})

	// Handle join form submission
	router.POST("/join", func(c *gin.Context) {
		// Get the authenticated user's Google ID
		session, sessionErr := store.Get(c.Request, "session-name")
		if sessionErr != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Not authenticated"})
			return
		}

		googleSub, ok := session.Values["google_sub"].(string)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Not authenticated"})
			return
		}

		// Log the Google sub ID and session data for debugging
		log.Printf("Join request - Google Sub ID: %s", googleSub)
		log.Printf("Session values: %+v", session.Values)

		// Get the email from session for comparison
		sessionEmail, _ := session.Values["email"].(string)
		log.Printf("Session email: %s", sessionEmail)

		var joinRequest struct {
			Name            string `json:"name"`
			Email           string `json:"email"`
			Plan            string `json:"plan"`
			Voice           string `json:"voice"`
			PaymentMethodID string `json:"payment_method_id"`
		}

		if err := c.BindJSON(&joinRequest); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request data"})
			return
		}

		// Log the join request data for debugging
		log.Printf("Join request data: %+v", joinRequest)

		// Compare session email with submitted email
		if sessionEmail != "" && sessionEmail != joinRequest.Email {
			log.Printf("WARNING: Email mismatch! Session email: %s, Submitted email: %s", sessionEmail, joinRequest.Email)
		}

		// Validate required fields
		if joinRequest.Name == "" || joinRequest.Email == "" || joinRequest.Plan == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "All fields are required"})
			return
		}

		// Check if user already exists as a speaker
		var exists bool
		err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM speakers WHERE google_id = $1)", googleSub).Scan(&exists)
		if err != nil {
			log.Printf("Error checking existing speaker: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
			return
		}

		log.Printf("Checking if user exists: google_id=%s, exists=%v", googleSub, exists)

		if exists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "You are already registered as a speaker"})
			return
		}

		// Check if email is already registered
		var emailExists bool
		err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM speakers WHERE email = $1)", joinRequest.Email).Scan(&emailExists)
		if err != nil {
			log.Printf("Error checking existing email: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
			return
		}

		log.Printf("Checking if email exists: email=%s, exists=%v", joinRequest.Email, emailExists)

		if emailExists {
			// Check if the email belongs to a different Google account
			var existingGoogleID string
			err = db.QueryRow("SELECT google_id FROM speakers WHERE email = $1", joinRequest.Email).Scan(&existingGoogleID)
			if err != nil {
				log.Printf("Error getting existing google_id for email: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
				return
			}

			if existingGoogleID != googleSub {
				c.JSON(http.StatusBadRequest, gin.H{"error": "This email address is already registered with a different Google account"})
				return
			} else {
				c.JSON(http.StatusBadRequest, gin.H{"error": "You are already registered as a speaker with this email"})
				return
			}
		}

		// Generate unique 5-digit speaker code
		speakerCode, err := generateUniqueSpeakerCode(db)
		if err != nil {
			log.Printf("Error generating speaker code: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate speaker code"})
			return
		}
		log.Printf("Generated speaker code: %s", speakerCode)

		// Create speaker record with Google ID
		var speakerID int
		log.Printf("Attempting to insert speaker: google_id=%s, name=%s, email=%s, speaker_code=%s",
			googleSub, joinRequest.Name, joinRequest.Email, speakerCode)

		err = db.QueryRow(`
			INSERT INTO speakers (google_id, name, email, speaker_code, payment_status, created_at)
			VALUES ($1, $2, $3, $4, 'pending', CURRENT_TIMESTAMP)
			RETURNING id
		`, googleSub, joinRequest.Name, joinRequest.Email, speakerCode).Scan(&speakerID)

		if err != nil {
			log.Printf("Error creating speaker: %v", err)
			log.Printf("Attempted to insert: google_id=%s, name=%s, email=%s, speaker_code=%s",
				googleSub, joinRequest.Name, joinRequest.Email, speakerCode)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create speaker account"})
			return
		}

		log.Printf("Successfully created speaker with ID: %d", speakerID)

		// Note: Preferred language is stored in the join request but not currently used
		// Could be added to speakers table later if needed

		// Create Stripe customer and subscription
		speaker := &Speaker{
			ID:            speakerID,
			GoogleID:      googleSub,
			Email:         joinRequest.Email,
			Name:          joinRequest.Name,
			SpeakerCode:   speakerCode,
			PaymentStatus: "pending",
		}

		paymentService := NewPaymentService(db)
		if err := paymentService.CreateStripeCustomer(speaker); err != nil {
			log.Printf("Error creating Stripe customer: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create payment account"})
			return
		}

		// Create subscription based on selected plan and voice preference
		subscriptionErr := createSubscriptionWithVoiceAddon(db, speaker, joinRequest.Plan, joinRequest.Voice, joinRequest.PaymentMethodID)
		if subscriptionErr != nil {
			log.Printf("Error creating subscription: %v", subscriptionErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create subscription"})
			return
		}

		// Check if subscription was created or skipped
		var paymentStatus string
		err = db.QueryRow("SELECT payment_status FROM speakers WHERE id = $1", speakerID).Scan(&paymentStatus)
		if err != nil {
			log.Printf("Error checking payment status: %v", err)
		}

		message := "Account created successfully"
		if paymentStatus == "pending_subscription" {
			message = "Account created successfully! Note: Subscription setup requires Stripe price IDs to be configured."
		}

		c.JSON(http.StatusOK, gin.H{
			"message":      message,
			"speaker_code": speakerCode,
		})
	})

	// Initialize payment service
	paymentService := NewPaymentService(db)

	// Add Stripe webhook endpoint
	router.POST("/webhook", func(c *gin.Context) {
		paymentService.HandleStripeWebhook(c.Writer, c.Request)
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

		// Create subscription if needed
		speaker := &Speaker{
			ID:                dbSpeakerID,
			GoogleID:          googleSub,
			Email:             "", // Will be updated from database
			Name:              speakerName,
			SpeakerCode:       speakerCode,
			CreatedAt:         time.Now(),
			PaymentStatus:     "active",
			SubscriptionID:    "",
			StripeCustomerID:  "",
			Conn:              conn,
			Language:          lang,
			LastActive:        time.Now(),
			DeepgramClient:    deepgramClient,
			DeepgramCtx:       ctx,
			DeepgramCancelCtx: cancel,
		}

		// Get speaker's email from database
		err = db.QueryRow("SELECT email FROM speakers WHERE id = $1", dbSpeakerID).Scan(&speaker.Email)
		if err != nil {
			log.Printf("Error getting speaker email: %v", err)
		}

		// Check if speaker already has a subscription
		var existingSubscriptionID string
		err = db.QueryRow("SELECT subscription_id FROM speakers WHERE id = $1", dbSpeakerID).Scan(&existingSubscriptionID)
		if err != nil && err != sql.ErrNoRows {
			log.Printf("Error checking existing subscription: %v", err)
		}

		// Create subscription only if one doesn't exist
		if existingSubscriptionID == "" {
			if err := createMeteredSubscription(db, speaker); err != nil {
				log.Printf("Error creating subscription: %v", err)
			}
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

			// Report usage to Stripe
			if err := ReportUsageToStripe(db, googleSub); err != nil {
				log.Printf("Error reporting usage to Stripe: %v", err)
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

// isPremiumSubscription checks if the user has a premium subscription
func isPremiumSubscription(planName string) bool {
	// Define premium plans - these get access to custom voices
	premiumPlans := map[string]bool{
		"yearly-150": true, // Enterprise Yearly (Premium)
	}

	return premiumPlans[planName]
}

// isProfessionalSubscription checks if the user has a professional subscription
func isProfessionalSubscription(planName string) bool {
	// Define professional plans - these get access to male/female ElevenLabs voices
	professionalPlans := map[string]bool{
		"monthly-8":  true, // Starter Monthly (Professional voice access)
		"yearly-100": true, // Professional Yearly
		"yearly-150": true, // Enterprise Yearly (also premium)
	}

	return professionalPlans[planName]
}

// getAvailableVoices returns available voices based on subscription tier
func getAvailableVoices(planName string, userVoiceID string) map[string]interface{} {
	voices := make(map[string]interface{})

	if isPremiumSubscription(planName) {
		// Premium users get all options - using ElevenLabs voice IDs
		voices["male"] = map[string]string{
			"default": "pqHfZKP75CvOlQylNhV4", // Male voice ID
		}
		voices["female"] = map[string]string{
			"default": "XrExE9yKIg1WjnnlVkGX", // Female voice ID
		}
		// Add custom voice if available
		if userVoiceID != "" {
			voices["custom"] = map[string]string{
				"custom": userVoiceID,
			}
		}
	} else if isProfessionalSubscription(planName) {
		// Professional users get male/female options but no custom voice
		voices["male"] = map[string]string{
			"default": "pqHfZKP75CvOlQylNhV4", // Male voice ID
		}
		voices["female"] = map[string]string{
			"default": "XrExE9yKIg1WjnnlVkGX", // Female voice ID
		}
	} else {
		// Starter users get default female voices only
		voices["female"] = map[string]string{
			"default": "XrExE9yKIg1WjnnlVkGX", // Female voice ID
		}
	}

	return voices
}

func handlePollyTTS(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		log.Println("=== TTS ENDPOINT CALLED ===")

		var req struct {
			Text        string `json:"text"`
			Language    string `json:"language"`
			VoiceId     string `json:"voiceId"`
			Speed       string `json:"speed"`
			VoiceType   string `json:"voiceType"`   // "male", "female", or "custom"
			SpeakerCode string `json:"speakerCode"` // Speaker code for audience requests
		}
		if err := c.BindJSON(&req); err != nil {
			log.Printf("Error binding JSON request: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
			return
		}

		log.Printf("TTS Request - Text: %q, Language: %s, VoiceID: %s, VoiceType: %s, SpeakerCode: %s", req.Text, req.Language, req.VoiceId, req.VoiceType, req.SpeakerCode)

		// Get speaker's subscription information
		// Priority: 1) speaker_code (for audience requests), 2) authenticated session (for speaker requests)
		var planName string
		var userVoiceID sql.NullString
		var voicePreference string
		session, _ := store.Get(c.Request, "session-name")
		googleSub, ok := session.Values["google_sub"].(string)

		if req.SpeakerCode != "" {
			// Audience request: look up speaker by speaker_code
			log.Printf("Looking up speaker by speaker_code: %s", req.SpeakerCode)
			err := db.QueryRow("SELECT plan_name, voice_id, voice_preference FROM speakers WHERE speaker_code = $1", req.SpeakerCode).Scan(&planName, &userVoiceID, &voicePreference)
			if err != nil {
				log.Printf("Error getting speaker subscription info by speaker_code: %v", err)
				// Continue with default behavior
				voicePreference = "polly" // Default fallback
			} else {
				log.Printf("Found speaker with voice_preference: %s, plan_name: %s", voicePreference, planName)
			}
		} else if ok {
			// Speaker request: get user's plan, voice_id, and voice_preference from database
			log.Printf("Looking up speaker by google_id: %s", googleSub)
			err := db.QueryRow("SELECT plan_name, voice_id, voice_preference FROM speakers WHERE google_id = $1", googleSub).Scan(&planName, &userVoiceID, &voicePreference)
			if err != nil {
				log.Printf("Error getting user subscription info: %v", err)
				// Continue with default behavior
				voicePreference = "polly" // Default fallback
			} else {
				log.Printf("Found user with voice_preference: %s, plan_name: %s", voicePreference, planName)
			}
		} else {
			log.Printf("No speaker_code or session found, defaulting to polly")
			voicePreference = "polly" // Default fallback
		}

		// Determine which TTS service to use based on subscription
		var audio []byte
		var err error
		var serviceUsed string

		// Determine voice service based on user's voice preference
		var selectedVoiceID string

		switch voicePreference {
		case "custom":
			// Custom voice users get their custom ElevenLabs voice
			if userVoiceID.Valid && userVoiceID.String != "" {
				selectedVoiceID = userVoiceID.String
				audio, err = translation.SynthesizeSpeech(req.Text, selectedVoiceID, req.Language)
				serviceUsed = "ElevenLabs (Custom Voice)"
			} else {
				// Fallback to default female voice if no custom voice ID
				selectedVoiceID = "XrExE9yKIg1WjnnlVkGX"
				audio, err = translation.SynthesizeSpeech(req.Text, selectedVoiceID, req.Language)
				serviceUsed = "ElevenLabs (Default)"
			}
		case "elevenlabs":
			// ElevenLabs users get male/female voice options
			if req.VoiceType == "male" {
				selectedVoiceID = "pqHfZKP75CvOlQylNhV4" // Male voice ID
			} else {
				selectedVoiceID = "XrExE9yKIg1WjnnlVkGX" // Female voice ID
			}
			audio, err = translation.SynthesizeSpeech(req.Text, selectedVoiceID, req.Language)
			serviceUsed = "ElevenLabs"
		case "polly", "":
			// Polly users (default) get AWS Polly
			log.Printf("Using AWS Polly TTS for user with voice preference: %s", voicePreference)
			audio, err = translation.SynthesizeSpeechWithPolly(req.Text, req.Language, req.VoiceId, req.Speed)
			serviceUsed = "AWS Polly"
		default:
			// Fallback to Polly for unknown voice preferences
			log.Printf("Unknown voice preference '%s', falling back to Polly", voicePreference)
			audio, err = translation.SynthesizeSpeechWithPolly(req.Text, req.Language, req.VoiceId, req.Speed)
			serviceUsed = "AWS Polly"
		}

		if err != nil {
			log.Printf("Error synthesizing speech with %s: %v", serviceUsed, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to synthesize speech"})
			return
		}

		log.Printf("Successfully received response from %s. Voice preference used: %s, Plan: %s", serviceUsed, voicePreference, planName)

		// Set headers for audio streaming
		c.Header("Content-Type", "audio/mpeg")
		c.Header("Content-Disposition", "attachment; filename=speech.mp3")
		c.Header("Transfer-Encoding", "chunked")

		// Stream the audio data to the client
		_, err = c.Writer.Write(audio)
		if err != nil {
			log.Printf("Error streaming audio: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to stream audio"})
			return
		}

		log.Printf("Successfully streamed audio to client using %s", serviceUsed)
		log.Println("=== TTS ENDPOINT COMPLETED ===")
	}
}

func handleDemoTTS(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		log.Println("=== DEMO TTS ENDPOINT CALLED ===")

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

		log.Printf("Demo TTS Request - Text: %q, Language: %s, VoiceID: %s", req.Text, req.Language, req.VoiceId)

		// For demo purposes, always use ElevenLabs with the provided voice ID
		audio, err := translation.SynthesizeSpeech(req.Text, req.VoiceId, req.Language)
		if err != nil {
			log.Printf("Error synthesizing speech with ElevenLabs: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to synthesize speech"})
			return
		}

		log.Printf("Successfully received response from ElevenLabs (Demo)")

		// Set headers for audio streaming
		c.Header("Content-Type", "audio/mpeg")
		c.Header("Content-Disposition", "attachment; filename=demo_speech.mp3")
		c.Header("Transfer-Encoding", "chunked")

		// Stream the audio data to the client
		_, err = c.Writer.Write(audio)
		if err != nil {
			log.Printf("Error streaming audio: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to stream audio"})
			return
		}

		log.Printf("Successfully streamed demo audio to client using ElevenLabs")
		log.Println("=== DEMO TTS ENDPOINT COMPLETED ===")
	}
}

// generateUniqueSpeakerCode generates a unique 5-digit speaker code
func generateUniqueSpeakerCode(db *sql.DB) (string, error) {
	maxAttempts := 100
	for i := 0; i < maxAttempts; i++ {
		// Generate a random 5-digit number
		code := fmt.Sprintf("%05d", time.Now().UnixNano()%100000)

		// Check if code already exists
		var exists bool
		err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM speakers WHERE speaker_code = $1)", code).Scan(&exists)
		if err != nil {
			return "", err
		}

		if !exists {
			return code, nil
		}

		// Small delay to ensure different timestamps
		time.Sleep(time.Millisecond)
	}

	return "", fmt.Errorf("failed to generate unique speaker code after %d attempts", maxAttempts)
}

// createSubscriptionWithVoiceAddon creates a subscription with both main plan and voice add-on
func createSubscriptionWithVoiceAddon(db *sql.DB, speaker *Speaker, plan, voice, paymentMethodID string) error {
	// Get the appropriate price ID based on the plan
	var priceID string
	switch plan {
	case "monthly-8":
		priceID = os.Getenv("STRIPE_MONTHLY_8_PRICE_ID")
	case "yearly-8":
		priceID = os.Getenv("STRIPE_YEARLY_8_PRICE_ID")
	case "yearly-50":
		priceID = os.Getenv("STRIPE_YEARLY_50_PRICE_ID")
	case "yearly-100":
		priceID = os.Getenv("STRIPE_YEARLY_100_PRICE_ID")
	case "yearly-150":
		priceID = os.Getenv("STRIPE_YEARLY_150_PRICE_ID")
	default:
		return fmt.Errorf("invalid plan: %s", plan)
	}

	if priceID == "" {
		log.Printf("Warning: Price ID not set for plan: %s, skipping subscription creation", plan)
		// Update speaker status to indicate no subscription
		_, err := db.Exec(`
			UPDATE speakers 
			SET payment_status = 'pending_subscription'
			WHERE id = $1
		`, speaker.ID)
		if err != nil {
			return fmt.Errorf("error updating speaker status: %v", err)
		}
		return nil // Don't fail the registration, just skip subscription
	}

	// Build subscription items - start with main plan
	subscriptionItems := []*stripe.SubscriptionItemsParams{
		{
			Price: stripe.String(priceID),
		},
	}

	// Add voice add-on if selected
	var voicePriceID string
	switch voice {
	case "elevenlabs":
		// Select the appropriate premium voice add-on based on plan hours
		switch plan {
		case "monthly-8":
			voicePriceID = os.Getenv("STRIPE_VOICE_ELEVENLABS_8H_PRICE_ID")
		case "yearly-8":
			voicePriceID = os.Getenv("STRIPE_VOICE_ELEVENLABS_8H_YEARLY_PRICE_ID")
		case "yearly-50":
			voicePriceID = os.Getenv("STRIPE_VOICE_ELEVENLABS_50H_PRICE_ID")
		case "yearly-100":
			voicePriceID = os.Getenv("STRIPE_VOICE_ELEVENLABS_100H_PRICE_ID")
		case "yearly-150":
			voicePriceID = os.Getenv("STRIPE_VOICE_ELEVENLABS_150H_PRICE_ID")
		}
	case "custom":
		// Select the appropriate custom voice add-on based on plan hours
		switch plan {
		case "monthly-8":
			voicePriceID = os.Getenv("STRIPE_VOICE_CUSTOM_8H_PRICE_ID")
		case "yearly-8":
			voicePriceID = os.Getenv("STRIPE_VOICE_CUSTOM_8H_YEARLY_PRICE_ID")
		case "yearly-50":
			voicePriceID = os.Getenv("STRIPE_VOICE_CUSTOM_50H_PRICE_ID")
		case "yearly-100":
			voicePriceID = os.Getenv("STRIPE_VOICE_CUSTOM_100H_PRICE_ID")
		case "yearly-150":
			voicePriceID = os.Getenv("STRIPE_VOICE_CUSTOM_150H_PRICE_ID")
		}
	}

	if voicePriceID != "" && voice != "polly" {
		subscriptionItems = append(subscriptionItems, &stripe.SubscriptionItemsParams{
			Price: stripe.String(voicePriceID),
		})
		log.Printf("Adding voice add-on: %s with price ID: %s", voice, voicePriceID)
	}

	// Create the subscription using Stripe with multiple items
	params := &stripe.SubscriptionParams{
		Customer:        stripe.String(speaker.StripeCustomerID),
		Items:           subscriptionItems,
		PaymentBehavior: stripe.String("default_incomplete"),
		PaymentSettings: &stripe.SubscriptionPaymentSettingsParams{
			PaymentMethodTypes: []*string{
				stripe.String("card"),
			},
		},
		Expand: []*string{
			stripe.String("latest_invoice.payment_intent"),
		},
	}

	subscription, err := subscription.New(params)
	if err != nil {
		return fmt.Errorf("error creating subscription: %v", err)
	}

	// Update speaker with subscription ID, plan name, and voice preference
	_, err = db.Exec(`
		UPDATE speakers 
		SET subscription_id = $1, payment_status = 'active', plan_name = $2, voice_preference = $3
		WHERE id = $4
	`, subscription.ID, plan, voice, speaker.ID)
	if err != nil {
		return fmt.Errorf("error updating speaker with subscription ID: %v", err)
	}

	speaker.SubscriptionID = subscription.ID
	speaker.PaymentStatus = "active"

	return nil
}

func handleGetAvailableVoices(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Get user from session
		session, _ := store.Get(c.Request, "session-name")
		googleSub, ok := session.Values["google_sub"].(string)
		if !ok {
			c.JSON(401, gin.H{"error": "Not authenticated"})
			return
		}

		// Get user's plan and voice_id from database
		var planName, userVoiceID string
		err := db.QueryRow("SELECT plan_name, voice_id FROM speakers WHERE google_id = $1", googleSub).Scan(&planName, &userVoiceID)
		if err != nil {
			if err == sql.ErrNoRows {
				c.JSON(404, gin.H{"error": "User not found"})
				return
			}
			log.Printf("Error getting user subscription info: %v", err)
			c.JSON(500, gin.H{"error": "Database error"})
			return
		}

		// Get available voices based on subscription tier
		voices := getAvailableVoices(planName, userVoiceID)

		c.JSON(200, gin.H{
			"voices":           voices,
			"plan":             planName,
			"has_custom_voice": userVoiceID != "",
		})
	}
}

func handleCreateElevenLabsVoice(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Parse multipart form
		audioFile, audioHeader, err := c.Request.FormFile("audio")
		if err != nil {
			c.JSON(400, gin.H{"error": "Missing audio file"})
			return
		}
		defer audioFile.Close()

		voiceName := c.PostForm("voice_name")
		if voiceName == "" {
			voiceName = "User Custom Voice"
		}
		description := c.PostForm("description")

		// Get user from session
		session, _ := store.Get(c.Request, "session-name")
		googleSub, ok := session.Values["google_sub"].(string)
		if !ok {
			c.JSON(401, gin.H{"error": "Not authenticated"})
			return
		}

		voiceID, err := translation.AddCustomVoice(audioFile, audioHeader, voiceName, description)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}

		// Store the voice_id in the speakers table
		_, err = db.Exec("UPDATE speakers SET voice_id = $1 WHERE google_id = $2", voiceID, googleSub)
		if err != nil {
			c.JSON(500, gin.H{"error": "Failed to save voice_id"})
			return
		}

		c.JSON(200, gin.H{"voice_id": voiceID})
	}
}

func handleDeleteAccount(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		log.Println("=== DELETE ACCOUNT ENDPOINT CALLED ===")

		// Get user from session
		session, _ := store.Get(c.Request, "session-name")
		googleSub, ok := session.Values["google_sub"].(string)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Not authenticated"})
			return
		}

		// Get user's voice_id and subscription_id from database
		var voiceID, subscriptionID, stripeCustomerID, planName sql.NullString
		err := db.QueryRow("SELECT voice_id, subscription_id, stripe_customer_id, plan_name FROM speakers WHERE google_id = $1", googleSub).Scan(&voiceID, &subscriptionID, &stripeCustomerID, &planName)
		if err != nil {
			if err == sql.ErrNoRows {
				c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
				return
			}
			log.Printf("Error getting user data: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
			return
		}

		// Log user type for debugging
		userType := "Regular"
		planNameStr := ""
		if planName.Valid {
			planNameStr = planName.String
			if isPremiumSubscription(planNameStr) {
				userType = "Premium"
			}
		}
		log.Printf("Deleting %s user account - Plan: %s, Voice ID: %s, Subscription ID: %s", userType, planNameStr, voiceID.String, subscriptionID.String)

		// 1. Cancel Stripe subscription if it exists
		if subscriptionID.Valid && subscriptionID.String != "" {
			log.Printf("Cancelling Stripe subscription: %s", subscriptionID.String)
			stripe.Key = os.Getenv("STRIPE_SECRET_KEY")

			// Cancel the subscription
			params := &stripe.SubscriptionParams{
				CancelAtPeriodEnd: stripe.Bool(true),
			}
			_, err := subscription.Update(subscriptionID.String, params)
			if err != nil {
				log.Printf("Error cancelling Stripe subscription: %v", err)
				// Continue with deletion even if subscription cancellation fails
			} else {
				log.Printf("Successfully cancelled Stripe subscription: %s", subscriptionID.String)
			}
		} else {
			log.Printf("No Stripe subscription found to cancel")
		}

		// 2. Delete custom voice from ElevenLabs if it exists (only for premium users)
		if voiceID.Valid && voiceID.String != "" {
			log.Printf("Deleting ElevenLabs voice: %s", voiceID.String)
			err := translation.DeleteCustomVoice(voiceID.String)
			if err != nil {
				log.Printf("Error deleting ElevenLabs voice: %v", err)
				// Continue with deletion even if voice deletion fails
			} else {
				log.Printf("Successfully deleted ElevenLabs voice: %s", voiceID.String)
			}
		} else {
			log.Printf("No custom voice found to delete (user may have basic subscription)")
		}

		// 3. Delete all related data from database
		// Start a transaction to ensure all deletions succeed or fail together
		tx, err := db.Begin()
		if err != nil {
			log.Printf("Error starting transaction: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Database error"})
			return
		}
		defer tx.Rollback() // Will be ignored if tx.Commit() is called

		// Delete speaking sessions
		_, err = tx.Exec("DELETE FROM speaking_sessions WHERE speaker_id = (SELECT id FROM speakers WHERE google_id = $1)", googleSub)
		if err != nil {
			log.Printf("Error deleting speaking sessions: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete speaking sessions"})
			return
		}

		// Delete the speaker record
		result, err := tx.Exec("DELETE FROM speakers WHERE google_id = $1", googleSub)
		if err != nil {
			log.Printf("Error deleting speaker: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete speaker account"})
			return
		}

		rowsAffected, err := result.RowsAffected()
		if err != nil {
			log.Printf("Error getting rows affected: %v", err)
		} else {
			log.Printf("Deleted %d speaker record(s)", rowsAffected)
		}

		// Commit the transaction
		if err := tx.Commit(); err != nil {
			log.Printf("Error committing transaction: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to commit database changes"})
			return
		}

		// 4. Clear the session
		session.Options.MaxAge = -1
		session.Save(c.Request, c.Writer)

		log.Printf("Successfully deleted account for user: %s", googleSub)
		log.Println("=== DELETE ACCOUNT ENDPOINT COMPLETED ===")

		c.JSON(http.StatusOK, gin.H{"message": "Account successfully deleted"})
	}
}
