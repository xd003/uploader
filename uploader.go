package main

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	telegramAPIURL = "https://api.telegram.org/bot"
	lastUploadTimestampFile = "/opt/docker/repos/musicbot/bot/last_upload.txt"
)

type TelegramResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	Result      struct {
		MessageID int    `json:"message_id"`
		FileID    string `json:"file_id"`
	} `json:"result"`
}

func writeLastUploadTime() error {
	// Ensure the directory exists
	err := os.MkdirAll(filepath.Dir(lastUploadTimestampFile), 0755)
	if err != nil {
		return fmt.Errorf("failed to create directory: %v", err)
	}

	// Write current timestamp to file
	currentTime := time.Now().Unix()
	return os.WriteFile(lastUploadTimestampFile, []byte(strconv.FormatInt(currentTime, 10)), 0644)
}

func checkAndWaitForDelay(delaySeconds int) error {
	// If no delay specified, return immediately
	if delaySeconds <= 0 {
		return nil
	}

	// Check if the last upload timestamp file exists
	data, err := os.ReadFile(lastUploadTimestampFile)
	if err != nil {
		// If file doesn't exist, it means no previous upload, so continue
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read last upload timestamp: %v", err)
	}

	// Parse the last upload timestamp
	lastUploadTime, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return fmt.Errorf("failed to parse last upload timestamp: %v", err)
	}

	// Calculate time since last upload
	timeSinceLastUpload := time.Since(time.Unix(lastUploadTime, 0))

	// If not enough time has passed, sleep
	if timeSinceLastUpload < time.Duration(delaySeconds)*time.Second {
		sleepDuration := time.Duration(delaySeconds)*time.Second - timeSinceLastUpload
		time.Sleep(sleepDuration)
	}

	return nil
}

func uploadFile(botToken, filePath, title, performer, thumbnailPath string, 
               chatID int64, duration, replyToMessageID int, parseMode string, delaySeconds int) (int, error) {
	// Check and wait for delay if specified
	if err := checkAndWaitForDelay(delaySeconds); err != nil {
		return 0, err
	}

	// Validate input file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return 0, fmt.Errorf("input file does not exist: %s", filePath)
	}

	// Determine file type based on extension
	fileExt := strings.ToLower(filepath.Ext(filePath))
	isAudio := fileExt != ".zip" && fileExt != ".rar" && fileExt != ".7z"
	
	// Choose the right API endpoint
	endpoint := "sendDocument"
	if isAudio {
		endpoint = "sendAudio"
	}

	file, err := os.Open(filePath)
	if err != nil {
		return 0, fmt.Errorf("failed to open file: %v", err)
	}
	defer file.Close()

	// Create a pipe to connect the file reader to the form writer
	pr, pw := io.Pipe()
	
	// Create multipart writer through the pipe writer
	multipartWriter := multipart.NewWriter(pw)
	
	// Start a goroutine to write the file data to the pipe
	go func() {
		var writeErr error
		
		defer func() {
			// Close the multipart writer first to finalize the form
			if closeErr := multipartWriter.Close(); closeErr != nil && writeErr == nil {
				writeErr = closeErr
			}
			
			// Close the pipe writer, propagating any error
			pw.CloseWithError(writeErr)
		}()
		
		// Add file with proper field name
		fieldName := "document"
		if isAudio {
			fieldName = "audio"
		}
		
		fileWriter, err := multipartWriter.CreateFormFile(fieldName, filepath.Base(filePath))
		if err != nil {
			writeErr = err
			return
		}
		
		// Copy file data
		if _, writeErr = io.Copy(fileWriter, file); writeErr != nil {
			return
		}
		
		// Add common metadata
		formFields := map[string]string{
			"chat_id": strconv.FormatInt(chatID, 10),
		}
		
		// Only add reply_to_message_id if it's not 0
		if replyToMessageID != 0 {
			formFields["reply_to_message_id"] = strconv.Itoa(replyToMessageID)
		}
		
		// Add parse_mode if provided
		if parseMode != "" {
			formFields["parse_mode"] = parseMode
		}
		
		// Add audio-specific metadata if it's an audio file
		if isAudio {
			if title != "" {
				formFields["title"] = title
			}
			
			if performer != "" {
				formFields["performer"] = performer
			}
			
			if duration > 0 {
				formFields["duration"] = strconv.Itoa(duration)
			}
			
			formFields["supports_streaming"] = "true"
		} else if title != "" { // For documents, use caption instead of title
			formFields["caption"] = title
		}
		
		for key, value := range formFields {
			if err := multipartWriter.WriteField(key, value); err != nil {
				writeErr = err
				return
			}
		}
		
		// Add thumbnail if provided
		if thumbnailPath != "" {
			if _, err := os.Stat(thumbnailPath); os.IsNotExist(err) {
				writeErr = fmt.Errorf("thumbnail file does not exist: %s", thumbnailPath)
				return
			}
			
			thumbnailFile, err := os.Open(thumbnailPath)
			if err != nil {
				writeErr = err
				return
			}
			defer thumbnailFile.Close()
			
			thumbPart, err := multipartWriter.CreateFormFile("thumb", filepath.Base(thumbnailPath))
			if err != nil {
				writeErr = err
				return
			}
			if _, err = io.Copy(thumbPart, thumbnailFile); err != nil {
				writeErr = err
				return
			}
		}
	}()
	
	// Create and send HTTP request
	url := fmt.Sprintf("%s%s/%s", telegramAPIURL, botToken, endpoint)
	req, err := http.NewRequest("POST", url, pr)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	
	// Set a longer timeout for large uploads
	client := &http.Client{
		Timeout: 10 * time.Minute,
	}

	// Send the request
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	// Decode response and return message ID
	var result TelegramResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("failed to decode response: %v", err)
	}
	
	if !result.OK {
		return 0, fmt.Errorf("telegram API error: %s", result.Description)
	}
	
	// Write the last upload timestamp
	if err := writeLastUploadTime(); err != nil {
		return 0, fmt.Errorf("failed to write last upload timestamp: %v", err)
	}
	
	return result.Result.MessageID, nil
}

func main() {
	if len(os.Args) < 8 {
		fmt.Fprintf(os.Stderr, "Usage: uploader <bot_token> <chat_id> <file_path> <title> <performer> <duration> <reply_to_message_id> [thumbnail_path] [parse_mode] [delay_seconds]\n")
		os.Exit(1)
	}
	
	botToken := os.Args[1]
	
	chatID, err := strconv.ParseInt(os.Args[2], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid chat ID: %v\n", err)
		os.Exit(1)
	}
	
	filePath := os.Args[3]
	title := os.Args[4]
	performer := os.Args[5]
	
	duration, err := strconv.Atoi(os.Args[6])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid duration: %v\n", err)
		os.Exit(1)
	}
	
	replyToMessageID, err := strconv.Atoi(os.Args[7])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid reply_to_message_id: %v\n", err)
		os.Exit(1)
	}
	
	thumbnailPath := ""
	if len(os.Args) > 8 {
		thumbnailPath = os.Args[8]
	}
	
	parseMode := ""
	if len(os.Args) > 9 {
		parseMode = os.Args[9]
	}
	
	delaySeconds := 0
	if len(os.Args) > 10 {
		delaySeconds, err = strconv.Atoi(os.Args[10])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid delay_seconds: %v\n", err)
			os.Exit(1)
		}
	}
	
	messageID, err := uploadFile(botToken, filePath, title, performer, thumbnailPath, chatID, duration, replyToMessageID, parseMode, delaySeconds)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error uploading file: %v\n", err)
		os.Exit(1)
	}
	
	// Print the message ID to stdout for capturing by calling program
	fmt.Println(messageID)
}
