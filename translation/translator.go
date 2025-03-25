package translation

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/translate"
)

type Translator struct {
	client *translate.Client
}

func NewTranslator() (*Translator, error) {
	log.Println("Initializing AWS Translate client...")
	
	// Get credentials from environment variables
	accessKeyID := os.Getenv("AWS_ACCESS_KEY_ID")
	secretAccessKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	region := os.Getenv("AWS_REGION")

	if accessKeyID == "" || secretAccessKey == "" || region == "" {
		return nil, fmt.Errorf("AWS credentials not found in environment variables")
	}

	// Create credentials provider
	creds := credentials.NewStaticCredentialsProvider(
		accessKeyID,
		secretAccessKey,
		"",
	)

	// Load configuration with credentials
	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion(region),
		config.WithCredentialsProvider(creds),
	)
	if err != nil {
		log.Printf("Error loading AWS config: %v", err)
		return nil, fmt.Errorf("unable to load AWS SDK config: %v", err)
	}

	client := translate.NewFromConfig(cfg)
	log.Println("AWS Translate client initialized successfully")
	return &Translator{
		client: client,
	}, nil
}

func (t *Translator) Translate(text, sourceLang, targetLang string) (string, error) {
	ctx := context.Background()
	log.Printf("Translating text from %s to %s: %s", sourceLang, targetLang, text)

	// Convert language codes to AWS format
	sourceLang = convertLangCode(sourceLang)
	targetLang = convertLangCode(targetLang)
	log.Printf("Converted language codes - Source: %s, Target: %s", sourceLang, targetLang)

	input := &translate.TranslateTextInput{
		Text:               &text,
		SourceLanguageCode: &sourceLang,
		TargetLanguageCode: &targetLang,
	}

	result, err := t.client.TranslateText(ctx, input)
	if err != nil {
		log.Printf("Translation error: %v", err)
		return "", fmt.Errorf("failed to translate text: %v", err)
	}

	if result.TranslatedText == nil {
		log.Println("No translation returned")
		return "", fmt.Errorf("no translation returned")
	}

	log.Printf("Translation successful: %s", *result.TranslatedText)
	return *result.TranslatedText, nil
}

func (t *Translator) Close() {
	// AWS client doesn't need explicit closing
}

// convertLangCode converts language codes to the format expected by AWS Translate
func convertLangCode(langCode string) string {
	// AWS Translate uses ISO 639-1 language codes
	// Remove the region code if present (e.g., "en-US" -> "en")
	if len(langCode) > 2 {
		return langCode[:2]
	}
	return langCode
} 