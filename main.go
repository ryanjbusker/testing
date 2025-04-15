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

	//AWS Transcribe
	"github.com/aws/aws-sdk-go-v2/service/transcribestreaming"
    transcribetypes "github.com/aws/aws-sdk-go-v2/service/transcribestreaming/types"
	// "github.com/aws/aws-sdk-go-v2/service/transcribestreaming"
    // tstream "github.com/aws/aws-sdk-go-v2/service/transcribestreaming/types"

	transcribeevent "github.com/aws/aws-sdk-go-v2/service/transcribestreaming/eventstream"
	transcribestreaming "github.com/aws/aws-sdk-go-v2/service/transcribestreaming"
    tstream "github.com/aws/aws-sdk-go-v2/service/transcribestreaming/types"
    transcribeevent "github.com/aws/aws-sdk-go-v2/service/transcribestreaming/eventstream"
)

var translator *translation.Translator
var transcribeClient *transcribestreaming.Client //AWS Transcribe
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins in development
	},
}

var pollyClient *polly.Client


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

	//AWS Transcribe
	transcribeClient = transcribestreaming.NewFromConfig(awsCfg)
	log.Println("AWS Transcribe client initialized successfully")

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

	//AWS Transcribe
	router.GET("/ws-audio", func(c *gin.Context) {
		handleAudioWebSocket(c)
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

// func handleAudioWebSocket(c *gin.Context) {
// 	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
// 	if err != nil {
// 		log.Printf("WebSocket upgrade failed: %v", err)
// 		return
// 	}
// 	defer conn.Close()

// 	// Configure transcription input
// 	// input := &transcribestreaming.StartStreamTranscriptionInput{
// 	// 	LanguageCode:         transcribetypes.LanguageCodeEnUS,
// 	// 	MediaEncoding:        transcribetypes.MediaEncodingPcm,
// 	// 	MediaSampleRateHertz: 16000,
// 	// }
// 	input := &transcribestreaming.StartStreamTranscriptionInput{
// 		LanguageCode:         "en-US",
// 		MediaEncoding:        tstream.MediaEncodingPcm,
// 		MediaSampleRateHertz: 16000,
// 	}
	
// 	// Create input/output handlers
// 	stream, err := transcribeClient.StartStreamTranscription(context.TODO(), input,
// 		transcribestreaming.WithReader(audioReader),   // Your mic audio stream
// 		transcribestreaming.WithEventStreamHandler(transcribestreaming.EventStreamHandlerFunc(handleTranscriptEvent)),
// 	)

// 	// Start AWS Transcribe streaming
// 	stream, err := transcribeClient.StartStreamTranscription(context.TODO(), input)
// 	if err != nil {
// 		log.Printf("Failed to start transcription stream: %v", err)
// 		return
// 	}
// 	defer stream.Close()

// 	// Start writer (audio to Transcribe)
// 	go func() {
// 		for {
// 			messageType, audioChunk, err := conn.ReadMessage()
// 			if err != nil {
// 				log.Printf("Audio read error: %v", err)
// 				break
// 			}
// 			if messageType != websocket.BinaryMessage {
// 				continue // ignore non-audio messages
// 			}
// 			err = stream.Writer.Write(audioChunk)
// 			if err != nil {
// 				log.Printf("Transcribe write error: %v", err)
// 				break
// 			}
// 		}
// 		stream.Writer.Close() // signal we're done sending
// 	}()

// 	// Read transcription results
// 	for event := range stream.TranscriptResultStream.Events() {
// 		switch e := event.(type) {
// 		case *transcribetypes.TranscriptEvent:
// 			for _, result := range e.Transcript.Results {
// 				if len(result.Alternatives) > 0 && !result.IsPartial {
// 					text := *result.Alternatives[0].Transcript
// 					log.Printf("AWS Transcribed: %s", text)

// 					// Translate + broadcast (just log for now)
// 					translatedText, err := translator.Translate(text, "en", "es") // temp: hardcoded "en" to "es"
// 					if err != nil {
// 						log.Printf("Translation failed: %v", err)
// 					} else {
// 						log.Printf("Translated: %s", translatedText)

// 						response := map[string]interface{}{
// 							"type":        "translation",
// 							"original":    text,
// 							"translation": translatedText,
// 						}
// 						respJSON, _ := json.Marshal(response)
// 						conn.WriteMessage(websocket.TextMessage, respJSON)
// 					}
// 				}
// 			}
// 		}
// 	}
// }

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

			if msgType, ok := data["type"].(string); ok && msgType == "speech" {
				text, ok := data["text"].(string)
				if !ok || text == "" {
					log.Printf("Received speech message with invalid or empty text")
					continue
				}

				speakerID := conn.RemoteAddr().String()
				mu.RLock() // Lock for reading speaker and audience data
				speaker := stream.Speakers[speakerID]

				if speaker == nil {
					mu.RUnlock()
					log.Printf("Received speech message from unknown speaker: %s", speakerID)
					continue // Ignore message if speaker is not found (already disconnected?)
				}

				// --- Translation Optimization ---
				// 1. Group audience by target language
				audienceByLang := make(map[string][]*websocket.Conn)
				for _, audience := range stream.Audience {
					audienceByLang[audience.Language] = append(audienceByLang[audience.Language], audience.Conn)
				}
				mu.RUnlock() // Unlock after reading audience data

				// 2. Translate once per target language
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

				// 3. Send translated text to relevant audience groups
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
			} else {
				// Handle other message types if necessary
				log.Printf("Received non-speech message or unknown type: %v", data)
			}
		} else {
			log.Printf("Received non-text message type: %d", messageType)
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

func handleAudioWebSocket(c *gin.Context) {
    lang := c.Query("lang")
    if lang == "" {
        lang = "en-US"
    }

    conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
    if err != nil {
        log.Printf("WebSocket upgrade failed: %v", err)
        return
    }

    reader, writer := io.Pipe()

    input := &transcribestreaming.StartStreamTranscriptionInput{
        LanguageCode:         transcribetypes.LanguageCode(lang),
        MediaEncoding:        transcribetypes.MediaEncodingPcm,
        // MediaSampleRateHertz: int32(16000),
		MediaSampleRateHertz: aws.Int32(16000),
    }

    go func() {
        _, err := transcribeClient.StartStreamTranscription(context.TODO(), input,
            transcribestreaming.WithReader(reader),
            transcribestreaming.WithEventStreamHandler(transcribestreaming.EventStreamHandlerFunc(
                func(ctx context.Context, stream transcribetypes.EventStream) error {
                    for event := range stream.Events() {
                        switch ev := event.(type) {
                        case *transcribetypes.TranscriptEvent:
                            for _, result := range ev.Transcript.Results {
                                if len(result.Alternatives) > 0 && !result.IsPartial {
                                    text := *result.Alternatives[0].Transcript
                                    log.Printf("AWS Transcribed: %s", text)

                                    translatedText, err := translator.Translate(text, "en", "es")
                                    if err != nil {
                                        log.Printf("Translation failed: %v", err)
                                        continue
                                    }

                                    response := map[string]interface{}{
                                        "type":        "translation",
                                        "original":    text,
                                        "translation": translatedText,
                                    }
                                    respJSON, _ := json.Marshal(response)
                                    conn.WriteMessage(websocket.TextMessage, respJSON)
                                }
                            }
                        }
                    }
                    return nil
                }),
            ),
        )
        if err != nil {
            log.Printf("Transcribe stream error: %v", err)
        }
    }()

    for {
        messageType, audioChunk, err := conn.ReadMessage()
        if err != nil {
            log.Printf("Audio read error: %v", err)
            break
        }
        if messageType != websocket.BinaryMessage {
            continue
        }
        _, err = writer.Write(audioChunk)
        if err != nil {
            log.Printf("Pipe write error: %v", err)
            break
        }
    }

    conn.Close()
    writer.Close()
    log.Printf("WebSocket and stream closed")
}

