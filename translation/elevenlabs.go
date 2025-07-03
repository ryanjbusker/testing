package translation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
)

const (
	// Set your ElevenLabs API key here or use an environment variable
	ElevenLabsAPIKeyEnv = "ELEVENLABS_API_KEY"
	ElevenLabsBaseURL   = "https://api.elevenlabs.io/v1"
)

// SynthesizeSpeech sends text to ElevenLabs TTS API and returns audio bytes
func SynthesizeSpeech(text, voiceID, languageCode string) ([]byte, error) {
	apiKey := os.Getenv(ElevenLabsAPIKeyEnv)
	if apiKey == "" {
		return nil, fmt.Errorf("ElevenLabs API key not set in environment variable %s", ElevenLabsAPIKeyEnv)
	}

	url := fmt.Sprintf("%s/text-to-speech/%s", ElevenLabsBaseURL, voiceID)

	payload := map[string]interface{}{
		"text":           text,
		"model_id":       "eleven_multilingual_v2", // Use multilingual model
		"voice_settings": map[string]interface{}{},
	}
	if languageCode != "" {
		payload["lang"] = languageCode
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ElevenLabs API error: %s", string(respBody))
	}

	audio, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return audio, nil
}

// SaveAudioToFile saves audio bytes to a file
func SaveAudioToFile(audio []byte, filename string) error {
	return os.WriteFile(filename, audio, 0644)
}

// AddCustomVoice uploads a voice sample to ElevenLabs and creates a custom voice.
// Returns the new Voice ID or an error.
func AddCustomVoice(audioFile multipart.File, audioFileHeader *multipart.FileHeader, voiceName, description string) (string, error) {
	apiKey := os.Getenv(ElevenLabsAPIKeyEnv)
	if apiKey == "" {
		return "", fmt.Errorf("ElevenLabs API key not set in environment variable %s", ElevenLabsAPIKeyEnv)
	}

	url := ElevenLabsBaseURL + "/voices/add"

	// Prepare multipart form data
	var b bytes.Buffer
	w := multipart.NewWriter(&b)

	// Add required fields
	_ = w.WriteField("name", voiceName)
	if description != "" {
		_ = w.WriteField("description", description)
	}

	// Add the audio file
	fw, err := w.CreateFormFile("files", audioFileHeader.Filename)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(fw, audioFile)
	if err != nil {
		return "", err
	}
	w.Close()

	req, err := http.NewRequest("POST", url, &b)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("xi-api-key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("ElevenLabs API error: %s", string(respBody))
	}

	// Parse response for voice_id
	var result struct {
		VoiceID string `json:"voice_id"`
	}
	err = json.Unmarshal(respBody, &result)
	if err != nil {
		return "", fmt.Errorf("Failed to parse ElevenLabs response: %v", err)
	}
	if result.VoiceID == "" {
		return "", fmt.Errorf("No voice_id returned from ElevenLabs")
	}
	return result.VoiceID, nil
}

// DeleteCustomVoice deletes a custom voice from ElevenLabs
func DeleteCustomVoice(voiceID string) error {
	apiKey := os.Getenv(ElevenLabsAPIKeyEnv)
	if apiKey == "" {
		return fmt.Errorf("ElevenLabs API key not set in environment variable %s", ElevenLabsAPIKeyEnv)
	}

	url := fmt.Sprintf("%s/voices/%s", ElevenLabsBaseURL, voiceID)

	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("xi-api-key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ElevenLabs API error: %s", string(respBody))
	}

	return nil
}

// Example usage (to be called from your main logic):
// text := "Hola, ¿cómo estás?"
// voiceID := "gOKbjKUxaOp1lU07V6Ap"
// languageCode := "es" // Spanish
// audio, err := SynthesizeSpeech(text, voiceID, languageCode)
// if err != nil {
//     log.Fatal(err)
// }
// err = SaveAudioToFile(audio, "output.mp3")
// if err != nil {
//     log.Fatal(err)
// }
