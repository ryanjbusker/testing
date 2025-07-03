package translation

import (
	"context"
	"fmt"
	"html"
	"io"
	"os"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/polly"
	"github.com/aws/aws-sdk-go-v2/service/polly/types"
)

const (
	// AWS Polly configuration
	PollyRegionEnv    = "TRANSLATE_REGION"
	PollyAccessKeyEnv = "TRANSLATE_ACCESS_KEY_ID"
	PollySecretKeyEnv = "TRANSLATE_SECRET_ACCESS_KEY"
)

// voiceSupportsNeural maps voice IDs to whether they support neural engine
var voiceSupportsNeural = map[string]bool{
	"Matthew":  true,
	"Brian":    true,
	"Joanna":   true,
	"Kevin":    true,
	"Kajal":    true,
	"Aria":     true,
	"Zayd":     true,
	"Arlet":    true,
	"Hiujin":   true,
	"Zhiyu":    true,
	"Jitka":    true,
	"Sofie":    true,
	"Laura":    true,
	"Suvi":     true,
	"Rémi":     true,
	"Isabelle": true,
	"Liam":     true,
	"Daniel":   true,
	"Hannah":   true,
	"Sabrina":  true,
	"Adriano":  true,
	"Takumi":   true,
	"Seoyeon":  true,
	"Ida":      true,
	"Ola":      true,
	"Thiago":   true,
	"Ines":     true,
	"Sergio":   true,
	"Andrés":   true,
	"Andres":   true,
	"Pedro":    true,
	"Elin":     true,
	"Burcu":    true,
}

// SynthesizeSpeechWithPolly sends text to AWS Polly TTS API and returns audio bytes
func SynthesizeSpeechWithPolly(text, languageCode, voiceId, speed string) ([]byte, error) {
	// Get AWS credentials from environment variables
	region := os.Getenv(PollyRegionEnv)
	accessKey := os.Getenv(PollyAccessKeyEnv)
	secretKey := os.Getenv(PollySecretKeyEnv)

	if region == "" || accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("AWS Polly credentials not set in environment variables")
	}

	// Create AWS configuration
	cfg, err := config.LoadDefaultConfig(context.Background(),
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

	// Create Polly client
	pollyClient := polly.NewFromConfig(cfg)

	// Determine voice ID
	var selectedVoiceId types.VoiceId
	if voiceId != "" {
		selectedVoiceId = types.VoiceId(voiceId)
	} else {
		// Map language codes to Polly voices
		voiceID := getPollyVoiceForLanguage(languageCode)
		selectedVoiceId = types.VoiceId(voiceID)
	}

	// Check if voice supports neural engine
	engine := types.EngineStandard
	if voiceSupportsNeural[string(selectedVoiceId)] {
		engine = types.EngineNeural
	}

	// Handle speed adjustment with SSML
	finalText := text
	isSSML := false

	if speed != "" && speed != "1" {
		speedFloat, err := strconv.ParseFloat(speed, 64)
		if err == nil {
			rate := fmt.Sprintf("%.0f%%", speedFloat*100)
			finalText = fmt.Sprintf(`<speak><prosody rate="%s">%s</prosody></speak>`, rate, html.EscapeString(text))
			isSSML = true
		}
	}

	// Create speech synthesis input
	input := &polly.SynthesizeSpeechInput{
		Text:         &finalText,
		OutputFormat: types.OutputFormatMp3,
		VoiceId:      selectedVoiceId,
		Engine:       engine,
	}

	if isSSML {
		input.TextType = types.TextTypeSsml
	}

	// Synthesize speech
	result, err := pollyClient.SynthesizeSpeech(context.Background(), input)
	if err != nil {
		return nil, fmt.Errorf("failed to synthesize speech with Polly: %v", err)
	}

	// Read audio data
	audio, err := io.ReadAll(result.AudioStream)
	if err != nil {
		return nil, fmt.Errorf("failed to read audio stream: %v", err)
	}

	return audio, nil
}

// getPollyVoiceForLanguage returns the appropriate Polly voice ID for the given language
func getPollyVoiceForLanguage(languageCode string) string {
	// Map language codes to Polly voice IDs
	voiceMap := map[string]string{
		"en":  "Joanna",  // English - Female
		"es":  "Lupe",    // Spanish - Female
		"fr":  "Lea",     // French - Female
		"de":  "Vicki",   // German - Female
		"it":  "Bianca",  // Italian - Female
		"pt":  "Camila",  // Portuguese - Female
		"ja":  "Mizuki",  // Japanese - Female
		"ko":  "Seoyeon", // Korean - Female
		"zh":  "Zhiyu",   // Chinese - Female
		"ru":  "Tatyana", // Russian - Female
		"ar":  "Zeina",   // Arabic - Female
		"hi":  "Aditi",   // Hindi - Female
		"nl":  "Lotte",   // Dutch - Female
		"sv":  "Elin",    // Swedish - Female
		"da":  "Naja",    // Danish - Female
		"no":  "Liv",     // Norwegian - Female
		"fi":  "Suvi",    // Finnish - Female
		"pl":  "Ewa",     // Polish - Female
		"tr":  "Filiz",   // Turkish - Female
		"he":  "Ilanit",  // Hebrew - Female
		"th":  "Somsi",   // Thai - Female
		"vi":  "Nhi",     // Vietnamese - Female
		"id":  "Siti",    // Indonesian - Female
		"ms":  "Zhiyu",   // Malay - Female (using Chinese voice as fallback)
		"tl":  "Zhiyu",   // Tagalog - Female (using Chinese voice as fallback)
		"el":  "Zhiyu",   // Greek - Female (using Chinese voice as fallback)
		"hu":  "Zhiyu",   // Hungarian - Female (using Chinese voice as fallback)
		"cs":  "Zhiyu",   // Czech - Female (using Chinese voice as fallback)
		"ro":  "Zhiyu",   // Romanian - Female (using Chinese voice as fallback)
		"sk":  "Zhiyu",   // Slovak - Female (using Chinese voice as fallback)
		"hr":  "Zhiyu",   // Croatian - Female (using Chinese voice as fallback)
		"sl":  "Zhiyu",   // Slovenian - Female (using Chinese voice as fallback)
		"et":  "Zhiyu",   // Estonian - Female (using Chinese voice as fallback)
		"lv":  "Zhiyu",   // Latvian - Female (using Chinese voice as fallback)
		"lt":  "Zhiyu",   // Lithuanian - Female (using Chinese voice as fallback)
		"bg":  "Zhiyu",   // Bulgarian - Female (using Chinese voice as fallback)
		"mk":  "Zhiyu",   // Macedonian - Female (using Chinese voice as fallback)
		"sq":  "Zhiyu",   // Albanian - Female (using Chinese voice as fallback)
		"sr":  "Zhiyu",   // Serbian - Female (using Chinese voice as fallback)
		"bs":  "Zhiyu",   // Bosnian - Female (using Chinese voice as fallback)
		"ca":  "Zhiyu",   // Catalan - Female (using Chinese voice as fallback)
		"eu":  "Zhiyu",   // Basque - Female (using Chinese voice as fallback)
		"gl":  "Zhiyu",   // Galician - Female (using Chinese voice as fallback)
		"is":  "Zhiyu",   // Icelandic - Female (using Chinese voice as fallback)
		"mt":  "Zhiyu",   // Maltese - Female (using Chinese voice as fallback)
		"cy":  "Zhiyu",   // Welsh - Female (using Chinese voice as fallback)
		"ga":  "Zhiyu",   // Irish - Female (using Chinese voice as fallback)
		"gd":  "Zhiyu",   // Scottish Gaelic - Female (using Chinese voice as fallback)
		"br":  "Zhiyu",   // Breton - Female (using Chinese voice as fallback)
		"fur": "Zhiyu",   // Friulian - Female (using Chinese voice as fallback)
		"oc":  "Zhiyu",   // Occitan - Female (using Chinese voice as fallback)
		"co":  "Zhiyu",   // Corsican - Female (using Chinese voice as fallback)
		"rm":  "Zhiyu",   // Romansh - Female (using Chinese voice as fallback)
		"lb":  "Zhiyu",   // Luxembourgish - Female (using Chinese voice as fallback)
		"af":  "Zhiyu",   // Afrikaans - Female (using Chinese voice as fallback)
		"sw":  "Zhiyu",   // Swahili - Female (using Chinese voice as fallback)
		"am":  "Zhiyu",   // Amharic - Female (using Chinese voice as fallback)
		"bn":  "Zhiyu",   // Bengali - Female (using Chinese voice as fallback)
		"gu":  "Zhiyu",   // Gujarati - Female (using Chinese voice as fallback)
		"kn":  "Zhiyu",   // Kannada - Female (using Chinese voice as fallback)
		"ml":  "Zhiyu",   // Malayalam - Female (using Chinese voice as fallback)
		"mr":  "Zhiyu",   // Marathi - Female (using Chinese voice as fallback)
		"ne":  "Zhiyu",   // Nepali - Female (using Chinese voice as fallback)
		"pa":  "Zhiyu",   // Punjabi - Female (using Chinese voice as fallback)
		"si":  "Zhiyu",   // Sinhala - Female (using Chinese voice as fallback)
		"ta":  "Zhiyu",   // Tamil - Female (using Chinese voice as fallback)
		"te":  "Zhiyu",   // Telugu - Female (using Chinese voice as fallback)
		"ur":  "Zhiyu",   // Urdu - Female (using Chinese voice as fallback)
		"fa":  "Zhiyu",   // Persian - Female (using Chinese voice as fallback)
		"ku":  "Zhiyu",   // Kurdish - Female (using Chinese voice as fallback)
		"ps":  "Zhiyu",   // Pashto - Female (using Chinese voice as fallback)
		"uz":  "Zhiyu",   // Uzbek - Female (using Chinese voice as fallback)
		"kk":  "Zhiyu",   // Kazakh - Female (using Chinese voice as fallback)
		"ky":  "Zhiyu",   // Kyrgyz - Female (using Chinese voice as fallback)
		"mn":  "Zhiyu",   // Mongolian - Female (using Chinese voice as fallback)
		"tg":  "Zhiyu",   // Tajik - Female (using Chinese voice as fallback)
		"tk":  "Zhiyu",   // Turkmen - Female (using Chinese voice as fallback)
		"az":  "Zhiyu",   // Azerbaijani - Female (using Chinese voice as fallback)
		"ka":  "Zhiyu",   // Georgian - Female (using Chinese voice as fallback)
		"hy":  "Zhiyu",   // Armenian - Female (using Chinese voice as fallback)
		"my":  "Zhiyu",   // Burmese - Female (using Chinese voice as fallback)
		"km":  "Zhiyu",   // Khmer - Female (using Chinese voice as fallback)
		"lo":  "Zhiyu",   // Lao - Female (using Chinese voice as fallback)
	}

	if voice, exists := voiceMap[languageCode]; exists {
		return voice
	}

	// Default to Joanna (English) if language not found
	return "Joanna"
}

// convertLanguageCodeForPolly converts language codes to Polly-compatible format
func convertLanguageCodeForPolly(languageCode string) string {
	// Polly uses specific language codes, map common ones
	languageMap := map[string]string{
		"en":  "en-US",
		"es":  "es-US",
		"fr":  "fr-FR",
		"de":  "de-DE",
		"it":  "it-IT",
		"pt":  "pt-BR",
		"ja":  "ja-JP",
		"ko":  "ko-KR",
		"zh":  "cmn-CN",
		"ru":  "ru-RU",
		"ar":  "arb",
		"hi":  "hi-IN",
		"nl":  "nl-NL",
		"sv":  "sv-SE",
		"da":  "da-DK",
		"no":  "nb-NO",
		"fi":  "fi-FI",
		"pl":  "pl-PL",
		"tr":  "tr-TR",
		"he":  "he-IL",
		"th":  "th-TH",
		"vi":  "vi-VN",
		"id":  "id-ID",
		"ms":  "ms-MY",
		"tl":  "fil-PH",
		"el":  "el-GR",
		"hu":  "hu-HU",
		"cs":  "cs-CZ",
		"ro":  "ro-RO",
		"sk":  "sk-SK",
		"hr":  "hr-HR",
		"sl":  "sl-SI",
		"et":  "et-EE",
		"lv":  "lv-LV",
		"lt":  "lt-LT",
		"bg":  "bg-BG",
		"mk":  "mk-MK",
		"sq":  "sq-AL",
		"sr":  "sr-RS",
		"bs":  "bs-BA",
		"ca":  "ca-ES",
		"eu":  "eu-ES",
		"gl":  "gl-ES",
		"is":  "is-IS",
		"mt":  "mt-MT",
		"cy":  "cy-GB",
		"ga":  "ga-IE",
		"gd":  "gd-GB",
		"br":  "br-FR",
		"fur": "fur-IT",
		"oc":  "oc-FR",
		"co":  "co-FR",
		"rm":  "rm-CH",
		"lb":  "lb-LU",
		"af":  "af-ZA",
		"sw":  "sw-KE",
		"am":  "am-ET",
		"bn":  "bn-IN",
		"gu":  "gu-IN",
		"kn":  "kn-IN",
		"ml":  "ml-IN",
		"mr":  "mr-IN",
		"ne":  "ne-NP",
		"pa":  "pa-IN",
		"si":  "si-LK",
		"ta":  "ta-IN",
		"te":  "te-IN",
		"ur":  "ur-PK",
		"fa":  "fa-IR",
		"ku":  "ku-TR",
		"ps":  "ps-AF",
		"uz":  "uz-UZ",
		"kk":  "kk-KZ",
		"ky":  "ky-KG",
		"mn":  "mn-MN",
		"tg":  "tg-TJ",
		"tk":  "tk-TM",
		"az":  "az-AZ",
		"ka":  "ka-GE",
		"hy":  "hy-AM",
		"my":  "my-MM",
		"km":  "km-KH",
		"lo":  "lo-LA",
	}

	if pollyCode, exists := languageMap[languageCode]; exists {
		return pollyCode
	}

	// Default to English if language not found
	return "en-US"
}
