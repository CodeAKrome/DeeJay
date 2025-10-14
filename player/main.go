package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	ollama "github.com/ollama/ollama/api"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	dbName         = "music"
	collectionName = "songs"
	pidFile        = "/tmp/dj.pid"
)

// Song represents the structure of a music file's metadata in MongoDB.
type Song struct {
	ID     string `bson:"_id"` // File path
	Title  string `bson:"title"`
	Artist string `bson:"artist"`
	Album  string `bson:"album"`
	Genre  string `bson:"genre"`
	Year   int    `bson:"year"`
}

// PlayerStats holds statistics for the player session.
type PlayerStats struct {
	mu                sync.Mutex
	StartTime         time.Time
	CommandsProcessed int64
	CommandsSucceeded int64
	CommandsFailed    int64
	SongsPlayed       int64
	TotalPlaybackTime time.Duration
}

// Global variables for stats and shutdown signal.
var stats = PlayerStats{StartTime: time.Now()}

// Channels for playback control.
var skipChan = make(chan struct{}, 1)
var stopChan = make(chan struct{}, 1)

// shutdown channel is now used for all forms of termination.
var shutdown = make(chan struct{})

// logFile is the file where logs will be written.
var logFile *os.File

func init() {
	var err error
	logFile, err = os.OpenFile("dj.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("FATAL: cannot open dj.log: %v", err)
	}
	log.SetOutput(io.MultiWriter(os.Stderr, logFile))
}

func main() {
	defer logFile.Close()
	if len(os.Args) < 2 {
		speak("You need to provide a command.")
		log.Fatalf("Usage: %s \"<command>\"", os.Args[0])
	}

	// Reconstruct the full command from all arguments
	command := strings.Join(os.Args[1:], " ")
	log.Printf("Received command: %q", command)
	log.Printf("Checking if starts with 'list ': %v", strings.HasPrefix(command, "list "))

	// Handle list commands
	if strings.HasPrefix(command, "list ") {
		log.Println("Routing to handleListCommand")
		handleListCommand(command)
		return
	}

	log.Println("Not a list command, continuing to normal flow")

	// Dispatch control commands to a running player instance.
	if os.Args[1] == "skip" || os.Args[1] == "stop" {
		handleControlCommand(os.Args[1])
		return
	}

	// Handle Ctrl-C for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2)
	go func() {
		for sig := range sigChan {
			switch sig {
			case syscall.SIGUSR1:
				log.Println("Received skip signal (SIGUSR1).")
				skipChan <- struct{}{}
			case syscall.SIGUSR2:
				log.Println("Received stop signal (SIGUSR2).")
				stopChan <- struct{}{}
			case os.Interrupt, syscall.SIGTERM:
				handleShutdown()
			}
		}
	}()

	stats.mu.Lock()
	stats.CommandsProcessed++
	stats.mu.Unlock()

	// --- MongoDB Connection ---
	mongoUser := os.Getenv("MONGO_USER")
	if mongoUser == "" {
		mongoUser = "root"
	}
	mongoPass := os.Getenv("MONGO_PASS")
	if mongoPass == "" {
		mongoPass = "example"
	}

	uri := fmt.Sprintf("mongodb://%s:%s@localhost:27017", mongoUser, mongoPass)
	mongoCtx, mongoCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer mongoCancel()

	client, err := mongo.Connect(mongoCtx, options.Client().ApplyURI(uri))
	if err != nil {
		speak("I could not connect to the music database.")
		log.Fatalf("Failed to connect to MongoDB: %v", err)
	}
	defer client.Disconnect(context.Background())

	ollamaCtx, ollamaCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer ollamaCancel()
	collection := client.Database(dbName).Collection(collectionName)

	// --- Parse Command and Execute ---
	filter, err := parseCommandWithOllama(ollamaCtx, command)
	if err != nil {
		stats.mu.Lock()
		stats.CommandsFailed++
		stats.mu.Unlock()
		speak("Sorry, I had trouble understanding that.")
		log.Printf("Ollama parsing error: %v", err)
		return
	}
	if len(filter) == 0 {
		speak("Sorry, I didn't understand that. Please try something like, play songs by Queen, or play music from 1999.")
		return
	}

	findOptions := options.Find()
	findOptions.SetSort(bson.D{{Key: "artist", Value: 1}, {Key: "year", Value: 1}, {Key: "album", Value: 1}, {Key: "track_num", Value: 1}})

	cursor, err := collection.Find(context.Background(), filter, findOptions)
	if err != nil {
		stats.mu.Lock()
		stats.CommandsFailed++
		stats.mu.Unlock()
		speak("I had trouble searching for that music.")
		log.Fatalf("Failed to query MongoDB: %v", err)
	}
	defer cursor.Close(context.Background())

	var songs []Song
	if err = cursor.All(context.Background(), &songs); err != nil {
		stats.mu.Lock()
		stats.CommandsFailed++
		stats.mu.Unlock()
		speak("I had trouble getting the song list.")
		log.Fatalf("Failed to decode songs: %v", err)
	}

	if len(songs) == 0 {
		speak("I couldn't find any music matching your request.")
		return
	}

	log.Println("Shuffling playlist...")
	rand.Shuffle(len(songs), func(i, j int) {
		songs[i], songs[j] = songs[j], songs[i]
	})

	stats.mu.Lock()
	stats.CommandsSucceeded++
	stats.mu.Unlock()

	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		log.Printf("Warning: could not write PID file: %v", err)
	}
	defer os.Remove(pidFile)

	playSongs(songs)
	printStatistics()
}

func handleListCommand(command string) {
	// Parse: "list artist <artist> [year]" or "list genre <genre> [year]"
	parts := strings.Fields(command)
	if len(parts) < 2 {
		log.Fatalf("Invalid list command. Usage: list artist <name> [year] or list genre <name> [year]")
	}

	listType := parts[1] // "artist" or "genre"
	if listType != "artist" && listType != "genre" {
		log.Fatalf("Invalid list type: %s. Must be 'artist' or 'genre'", listType)
	}

	// If only "list genre" or "list artist" with no more args
	if len(parts) < 3 {
		log.Fatalf("Invalid list command. Usage: list artist <name> [year] or list genre <name> [year]")
	}

	// Extract the name (handle quoted names)
	nameStart := strings.Index(command, parts[2])
	remainder := command[nameStart:]

	var name string
	var yearStr string

	// Check if the next token looks like a year (4 digits or range)
	isYear := func(s string) bool {
		// Check for year range (1983-1985)
		if strings.Contains(s, "-") {
			parts := strings.Split(s, "-")
			if len(parts) == 2 {
				_, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
				_, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
				return err1 == nil && err2 == nil
			}
		}
		// Check for single year
		if year, err := strconv.Atoi(s); err == nil && year >= 1000 && year <= 9999 {
			return true
		}
		return false
	}

	// Check if name is quoted
	if strings.HasPrefix(remainder, "\"") {
		endQuote := strings.Index(remainder[1:], "\"")
		if endQuote == -1 {
			log.Fatalf("Unclosed quote in list command")
		}
		name = remainder[1 : endQuote+1]
		if len(remainder) > endQuote+2 {
			yearStr = strings.TrimSpace(remainder[endQuote+2:])
		}
	} else {
		// Check if the first token is a year
		tokens := strings.Fields(remainder)
		if isYear(tokens[0]) {
			// First token is a year, so no name filter
			yearStr = tokens[0]
			name = ""
		} else {
			// First token is the name
			name = tokens[0]
			if len(tokens) > 1 {
				yearStr = tokens[1]
			}
		}
	}

	log.Printf("Parsed - listType: %s, name: %q, yearStr: %q", listType, name, yearStr)

	// Connect to MongoDB
	mongoUser := os.Getenv("MONGO_USER")
	if mongoUser == "" {
		mongoUser = "root"
	}
	mongoPass := os.Getenv("MONGO_PASS")
	if mongoPass == "" {
		mongoPass = "example"
	}

	uri := fmt.Sprintf("mongodb://%s:%s@localhost:27017", mongoUser, mongoPass)
	mongoCtx, mongoCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer mongoCancel()

	client, err := mongo.Connect(mongoCtx, options.Client().ApplyURI(uri))
	if err != nil {
		log.Fatalf("Failed to connect to MongoDB: %v", err)
	}
	defer client.Disconnect(context.Background())

	collection := client.Database(dbName).Collection(collectionName)

	// Build filter
	filter := bson.M{}
	if name != "" {
		if listType == "artist" {
			filter["artist"] = primitive.Regex{Pattern: name, Options: "i"}
		} else {
			filter["genre"] = primitive.Regex{Pattern: name, Options: "i"}
		}
	}
	// If name is empty, we list all songs (optionally filtered by year)

	// Handle year or year range
	if yearStr != "" {
		if strings.Contains(yearStr, "-") {
			// Year range: 1982-1985
			years := strings.Split(yearStr, "-")
			if len(years) == 2 {
				startYear, err1 := strconv.Atoi(strings.TrimSpace(years[0]))
				endYear, err2 := strconv.Atoi(strings.TrimSpace(years[1]))
				if err1 == nil && err2 == nil {
					filter["year"] = bson.M{
						"$gte": startYear,
						"$lte": endYear,
					}
					log.Printf("Using year range filter: %d-%d", startYear, endYear)
				}
			}
		} else {
			// Single year
			year, err := strconv.Atoi(strings.TrimSpace(yearStr))
			if err == nil {
				filter["year"] = year
				log.Printf("Using year filter: %d", year)
			} else {
				log.Printf("Warning: could not parse year: %s", yearStr)
			}
		}
	}

	log.Printf("Generated filter: %+v", filter)

	// Query with sorting
	findOptions := options.Find()
	findOptions.SetSort(bson.D{{Key: "year", Value: 1}, {Key: "artist", Value: 1}, {Key: "album", Value: 1}, {Key: "track_num", Value: 1}})

	cursor, err := collection.Find(context.Background(), filter, findOptions)
	if err != nil {
		log.Fatalf("Failed to query MongoDB: %v", err)
	}
	defer cursor.Close(context.Background())

	var songs []Song
	if err = cursor.All(context.Background(), &songs); err != nil {
		log.Fatalf("Failed to decode songs: %v", err)
	}

	if len(songs) == 0 {
		log.Printf("No songs found matching criteria")
		return
	}

	// Output TSV format
	if listType == "artist" {
		// Format: year\tartist\talbum\ttitle
		for _, song := range songs {
			fmt.Printf("%d\t%s\t%s\t%s\n", song.Year, song.Artist, song.Album, song.Title)
		}
	} else {
		// Format: year\tgenre\tartist\talbum\ttitle
		for _, song := range songs {
			fmt.Printf("%d\t%s\t%s\t%s\t%s\n", song.Year, song.Genre, song.Artist, song.Album, song.Title)
		}
	}
}

func killAllAfplay() {
	cmd := exec.Command("pkill", "-9", "-f", "afplay")
	_ = cmd.Run()
}

func handleShutdown() {
	log.Println("\nReceived interrupt signal. Stopping playback...")
	speak("Stopping playback.")
	close(shutdown)
}

func speak(text string) {
	log.Printf("SAY: %s", text)
	fmt.Println(text)
	cmd := exec.Command("say", text)
	cmd.Run()
}

func playSongs(songs []Song) {
	count := len(songs)
	var response string
	if count == 1 {
		response = fmt.Sprintf("Now playing %s by %s.", songs[0].Title, songs[0].Artist)
	} else {
		response = fmt.Sprintf("Now playing %d songs.", count)
	}
	speak(response)

	killAllAfplay()

	var currentPlaybackCmd *exec.Cmd
	var playbackDone = make(chan error, 1)

	for i, song := range songs {
		select {
		case <-shutdown:
			log.Println("Shutdown signal received, exiting playback loop.")
			return
		default:
		}

		log.Printf("Playing (%d/%d): %s - %s (%s)", i+1, count, song.Artist, song.Title, song.ID)
		killAllAfplay()

		currentPlaybackCmd = exec.Command("afplay", song.ID)
		currentPlaybackCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		startTime := time.Now()
		if err := currentPlaybackCmd.Start(); err != nil {
			log.Printf("Error starting afplay for %s: %v", song.ID, err)
			continue
		}

		go func() {
			playbackDone <- currentPlaybackCmd.Wait()
		}()

		var err error
		skipped := false

		select {
		case <-shutdown:
			log.Println("Playback interrupted by shutdown signal.")
			if currentPlaybackCmd.Process != nil && currentPlaybackCmd.Process.Pid > 0 {
				syscall.Kill(-currentPlaybackCmd.Process.Pid, syscall.SIGKILL)
				currentPlaybackCmd.Wait()
			}
			return

		case <-stopChan:
			log.Println("Stopping playlist.")
			if currentPlaybackCmd.Process != nil && currentPlaybackCmd.Process.Pid > 0 {
				syscall.Kill(-currentPlaybackCmd.Process.Pid, syscall.SIGKILL)
				currentPlaybackCmd.Wait()
			}
			return

		case <-skipChan:
			log.Println("Skipping to next song.")
			if currentPlaybackCmd.Process != nil && currentPlaybackCmd.Process.Pid > 0 {
				syscall.Kill(-currentPlaybackCmd.Process.Pid, syscall.SIGKILL)

				done := make(chan struct{})
				go func() {
					currentPlaybackCmd.Wait()
					close(done)
				}()
				select {
				case <-done:
					log.Println("Previous afplay process exited cleanly.")
				case <-time.After(500 * time.Millisecond):
					log.Println("Timeout waiting for afplay to exit, forcing cleanup.")
					exec.Command("pkill", "-9", "-f", "afplay").Run()
				}
			}

			// Drain any additional skip signals
			for len(skipChan) > 0 {
				<-skipChan
			}

			// Drain the playbackDone channel to consume the kill error
			select {
			case <-playbackDone:
			case <-time.After(100 * time.Millisecond):
			}

			skipped = true

		case err = <-playbackDone:
		}

		// Only process playback results if the song wasn't skipped
		if !skipped {
			playbackDuration := time.Since(startTime)

			if err != nil {
				log.Printf("Error playing %s: %v", song.ID, err)
			} else {
				stats.mu.Lock()
				stats.SongsPlayed++
				stats.TotalPlaybackTime += playbackDuration
				stats.mu.Unlock()
			}
		}
	}

	log.Println("Playlist finished.")
}

func handleControlCommand(command string) {
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		log.Fatalf("Could not read PID file. Is a song playing? Error: %v", err)
	}

	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		log.Fatalf("Invalid PID found in PID file: %v", err)
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		log.Fatalf("Could not find running player process with PID %d: %v", pid, err)
	}

	var sig syscall.Signal
	switch command {
	case "skip":
		sig = syscall.SIGUSR1
		speak("Skipping.")
	case "stop":
		sig = syscall.SIGUSR2
		speak("Stopping.")
	default:
		log.Fatalf("Unknown control command: %s", command)
	}

	if err := process.Signal(sig); err != nil {
		log.Fatalf("Failed to send %s signal to process %d: %v", command, pid, err)
	}

	log.Printf("Successfully sent %s signal to process %d.", command, pid)
}

func printStatistics() {
	stats.mu.Lock()
	defer stats.mu.Unlock()

	fmt.Printf("\n--- Playback Session Statistics ---\n")
	fmt.Printf("  Session Duration:   %v\n", time.Since(stats.StartTime).Round(time.Second))
	fmt.Printf("  Commands Processed: %d\n", stats.CommandsProcessed)
	fmt.Printf("  - Succeeded:        %d\n", stats.CommandsSucceeded)
	fmt.Printf("  - Failed:           %d\n", stats.CommandsFailed)
	fmt.Printf("  Songs Completed:    %d\n", stats.SongsPlayed)
	fmt.Printf("  Total Playback Time: %v\n", stats.TotalPlaybackTime.Round(time.Second))
	fmt.Println("-----------------------------------")
}

const toolDefinition = `
{
  "name": "create_playlist",
  "description": "Create a playlist based on user specifications for artist, album, title, genre, or year/year range.",
  "parameters": {
    "type": "object",
    "properties": {
      "artist": {
        "type": "string",
        "description": "The name of the artist or band."
      },
      "album": {
        "type": "string",
        "description": "The name of the album."
      },
      "title": {
        "type": "string",
        "description": "The title of the song."
      },
      "genre": {
        "type": "string",
        "description": "The genre of the music, for example 'Rock', 'Pop', or 'Jazz'."
      },
      "year_start": {
        "type": "number",
        "description": "The starting year for a year range, or the specific year if no end year is given."
      },
      "year_end": {
        "type": "number",
        "description": "The ending year for a year range. Only include if the user specifies a range."
      }
    },
    "required": []
  }
}
`

func parseCommandWithOllama(ctx context.Context, command string) (bson.M, error) {
	client, err := ollama.ClientFromEnvironment()
	if err != nil {
		return nil, fmt.Errorf("could not create ollama client: %w", err)
	}

	prompt := fmt.Sprintf(`You are a music selection assistant. Analyze the user's request and respond with ONLY a JSON object in this exact format:

{
  "name": "create_playlist",
  "parameters": {
    "artist": "artist name here or empty string",
    "album": "album name here or empty string",
    "title": "song title here or empty string",
    "genre": "genre here or empty string",
    "year_start": year_number_or_null,
    "year_end": year_number_or_null
  }
}

Rules:
- Use empty strings ("") for text fields that aren't specified
- Use null for year fields that aren't specified
- If a year range is given (like 1982-1983), put the first year in year_start and second in year_end
- If only one year is given, put it in year_start and leave year_end as null
- Do not include any other text, explanations, or formatting

User request: '%s'`, command)

	req := &ollama.GenerateRequest{
		Model:  "llama3.1:8b",
		Prompt: prompt,
		Format: json.RawMessage(`"json"`),
		System: "You are a JSON formatter. Output only valid JSON matching the requested schema. No explanations.",
	}

	var responseText string
	respFunc := func(resp ollama.GenerateResponse) error {
		responseText += resp.Response
		return nil
	}

	err = client.Generate(ctx, req, respFunc)
	if err != nil {
		return nil, fmt.Errorf("ollama generation failed: %w", err)
	}

	log.Printf("Ollama raw response: %s", responseText)

	var toolCall struct {
		Name       string                 `json:"name"`
		Parameters map[string]interface{} `json:"parameters"`
	}

	cleanedJSON := strings.Trim(responseText, " \n\t`")
	if strings.HasPrefix(cleanedJSON, "json") {
		cleanedJSON = strings.TrimPrefix(cleanedJSON, "json")
	}
	cleanedJSON = strings.Trim(cleanedJSON, " \n\t`")

	if err := json.Unmarshal([]byte(cleanedJSON), &toolCall); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ollama response: %w. Response was: %s", err, cleanedJSON)
	}

	if toolCall.Name != "create_playlist" || toolCall.Parameters == nil {
		return nil, fmt.Errorf("model did not return a valid create_playlist tool call. Received: %s", cleanedJSON)
	}

	filter := bson.M{}

	for key, value := range toolCall.Parameters {
		if value == nil || value == "" {
			continue
		}

		switch key {
		case "artist", "album", "title", "genre":
			if strVal, ok := value.(string); ok && strVal != "" {
				filter[key] = primitive.Regex{Pattern: strVal, Options: "i"}
			}
		case "year_start":
			var yearStart int
			if floatVal, ok := value.(float64); ok {
				yearStart = int(floatVal)
			} else if strVal, ok := value.(string); ok {
				if y, err := strconv.Atoi(strVal); err == nil {
					yearStart = y
				}
			}

			if yearStart > 0 {
				// Check if we also have year_end
				if yearEndVal, hasEnd := toolCall.Parameters["year_end"]; hasEnd && yearEndVal != nil {
					var yearEnd int
					if floatVal, ok := yearEndVal.(float64); ok {
						yearEnd = int(floatVal)
					} else if strVal, ok := yearEndVal.(string); ok {
						if y, err := strconv.Atoi(strVal); err == nil {
							yearEnd = y
						}
					}

					if yearEnd > 0 {
						// Year range query
						filter["year"] = bson.M{
							"$gte": yearStart,
							"$lte": yearEnd,
						}
						log.Printf("Using year range filter: %d-%d", yearStart, yearEnd)
					} else {
						// Just start year
						filter["year"] = yearStart
					}
				} else {
					// Just start year
					filter["year"] = yearStart
				}
			}
		case "year_end":
			// Already handled in year_start case
			continue
		}
	}

	log.Printf("Generated MongoDB filter: %+v", filter)
	return filter, nil
}

/*
--- How to Build and Install ---
1. cd /Users/kyle/hub/DeeJay/player
2. go build .
3. sudo mv player /usr/local/bin/dj

--- Example Commands ---
dj "play songs by Queen"
dj "play music from 1999"
dj "play music from 1982 to 1985"
dj "play rock music from the 80s"
dj "play the song called \"Bohemian Rhapsody\""
dj "play rock music by led zeppelin"
dj "play songs by \"Daft Punk\" from the album \"Discovery\""
dj "play music by other from 1982-1983"
dj "play jazz from 1950 to 1960"

--- List Commands (TSV output) ---
dj "list artist Queen"
dj "list artist \"Queen\" 1975"
dj "list artist Queen 1975-1980"
dj "list genre Rock"
dj "list genre Jazz 1950-1960"
dj "list genre 1983"
dj "list artist 1983-1985"
*/
