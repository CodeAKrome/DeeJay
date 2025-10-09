package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
// This should be identical to the one in your indexer.
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

func main() {
	if len(os.Args) < 2 {
		// Using `say` for voice feedback on error
		speak("You need to provide a command.")
		log.Fatalf("Usage: %s \"<command>\"", os.Args[0])
	}
	command := os.Args[1]

	// Dispatch control commands to a running player instance.
	if command == "skip" || command == "stop" {
		handleControlCommand(command)
		return
	}

	// Handle Ctrl-C for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	// Listen for interrupt, term (for graceful shutdown), and our custom signals.
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
	// Short context for quick DB connection check
	mongoCtx, mongoCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer mongoCancel()

	client, err := mongo.Connect(mongoCtx, options.Client().ApplyURI(uri))
	if err != nil {
		speak("I could not connect to the music database.")
		log.Fatalf("Failed to connect to MongoDB: %v", err)
	}
	defer client.Disconnect(context.Background())

	// Create a new, longer context for the Ollama call
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

	stats.mu.Lock()
	stats.CommandsSucceeded++
	stats.mu.Unlock()

	// Write PID file to allow other instances to control this one.
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		log.Printf("Warning: could not write PID file: %v", err)
	}
	defer os.Remove(pidFile)

	// --- Play Music ---
	playSongs(songs)

	// Print stats at the end of a successful run or graceful shutdown.
	printStatistics()
}

// handleShutdown centralizes the shutdown logic.
func handleShutdown() {
	log.Println("\nReceived interrupt signal. Stopping playback...")
	speak("Stopping playback.")
	close(shutdown) // Signal all goroutines to stop.
}

// speak uses the macOS `say` command to provide voice feedback.
func speak(text string) {
	// Also print to stdout for the Siri Shortcut to capture
	fmt.Println(text)
	cmd := exec.Command("say", text)
	cmd.Run()
}

// playSongs iterates through a list of songs and plays them using `afplay`.
func playSongs(songs []Song) {
	count := len(songs)
	var response string
	if count == 1 {
		response = fmt.Sprintf("Now playing %s by %s.", songs[0].Title, songs[0].Artist)
	} else {
		response = fmt.Sprintf("Now playing %d songs.", count)
	}
	speak(response)

	var currentPlaybackCmd *exec.Cmd
	var playbackDone = make(chan error, 1)

	for i, song := range songs {
		select {
		case <-shutdown:
			log.Println("Shutdown signal received, exiting playback loop.")
			return
		default:
			// Continue to next song
		}

		log.Printf("Playing (%d/%d): %s - %s (%s)", i+1, count, song.Artist, song.Title, song.ID)
		currentPlaybackCmd = exec.Command("afplay", song.ID)

		// Run the song in a goroutine so we don't block.
		startTime := time.Now()
		go func() {
			playbackDone <- currentPlaybackCmd.Run()
		}()

		// Wait for the song to finish, or for a control signal.
		var err error
		interrupted := false
		select {
		case <-shutdown:
			log.Println("Playback interrupted by shutdown signal.")
			if currentPlaybackCmd.Process != nil {
				currentPlaybackCmd.Process.Kill()
			}
			return
		case <-stopChan:
			log.Println("Stopping playlist.")
			if currentPlaybackCmd.Process != nil {
				currentPlaybackCmd.Process.Kill()
			}
			return
		case <-skipChan:
			log.Println("Skipping to next song.")
			if currentPlaybackCmd.Process != nil {
				currentPlaybackCmd.Process.Kill()
			}
			interrupted = true
			// Continue to the next iteration of the loop.
			continue
		case err = <-playbackDone:
			// Song finished normally.
		}

		playbackDuration := time.Since(startTime)

		if err != nil && !interrupted {
			// Log error only if it wasn't an intentional interruption.
			log.Printf("Error playing %s: %v", song.ID, err)
		} else if err == nil {
			// Song completed successfully
			stats.mu.Lock()
			stats.SongsPlayed++
			stats.TotalPlaybackTime += playbackDuration
			stats.mu.Unlock()
		}
	}
	log.Println("Playlist finished.")
}

// handleControlCommand sends a signal to the running player process.
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

// printStatistics prints a summary report of the session.
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

// Tool definition for the Ollama model
const toolDefinition = `
{
  "name": "create_playlist",
  "description": "Create a playlist based on user specifications for artist, album, title, genre, or year.",
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
      "year": {
        "type": "number",
        "description": "The release year of the music."
      }
    },
    "required": []
  }
}
`

// parseCommandWithOllama uses a local Ollama model to convert a text command into a MongoDB filter.
func parseCommandWithOllama(ctx context.Context, command string) (bson.M, error) {
	// Connect to the local Ollama server
	client, err := ollama.ClientFromEnvironment()
	if err != nil {
		return nil, fmt.Errorf("could not create ollama client: %w", err)
	}

	// Prepare the prompt for the model
	prompt := fmt.Sprintf("You are a music selection assistant. Your task is to analyze the user's request and call the `create_playlist` tool with the appropriate parameters. Only respond with the JSON for the tool call. User request: '%s'", command)

	req := &ollama.GenerateRequest{
		Model:  "deepseek-r1:70b", // Using gemma:2b as a fast and capable model. Change to "gpt-oss:20b" if you have it.
		Prompt: prompt,
		Format: json.RawMessage(`"json"`), // Instruct Ollama to output JSON
		System: "You are a helpful assistant that extracts information from a user's request and formats it as a JSON tool call. The tool you have available is: " + toolDefinition,
	}

	var responseText string
	respFunc := func(resp ollama.GenerateResponse) error {
		responseText += resp.Response
		return nil
	}

	// Call the Ollama API
	err = client.Generate(ctx, req, respFunc)
	if err != nil {
		return nil, fmt.Errorf("ollama generation failed: %w", err)
	}

	// Print the raw response from Ollama for debugging purposes.
	log.Printf("Ollama raw response: %s", responseText)

	// The model should return a JSON object representing the tool call.
	// Example: {"name": "create_playlist", "arguments": {"artist": "Queen"}}
	var toolCall struct {
		Name       string                 `json:"name"`
		Parameters map[string]interface{} `json:"parameters"`
	}

	// The model's output might be wrapped in markdown, so we clean it.
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

	// Convert the extracted arguments into a MongoDB filter
	filter := bson.M{}
	for key, value := range toolCall.Parameters {
		if value == "" {
			continue
		}
		switch key {
		case "artist", "album", "title", "genre":
			// Use a case-insensitive regex for flexible matching
			if strVal, ok := value.(string); ok {
				filter[key] = primitive.Regex{Pattern: strVal, Options: "i"}
			}
		case "year":
			// The model might return year as a number (float64) or a string.
			// We need to handle both cases.
			if floatVal, ok := value.(float64); ok {
				filter["year"] = int(floatVal)
			} else if strVal, ok := value.(string); ok {
				if year, err := strconv.Atoi(strVal); err == nil {
					filter["year"] = year
				}
			}
		}
	}

	return filter, nil
}

/*
--- How to Build and Install ---
1. cd /Users/kyle/hub/DeeJay/player
2. go build .
3. sudo mv player /usr/local/bin/dj
   (This makes the command `dj` available system-wide)

--- Example Commands ---
dj "play songs by Queen"
dj "play music from 1999"
dj "play the song called \"Bohemian Rhapsody\""
dj "play rock music by led zeppelin"
dj "play songs by \"Daft Punk\" from the album \"Discovery\""
*/

/*
--- Siri Shortcut Setup ---
1. Open the Shortcuts app on your Mac.
2. Create a new Shortcut. Name it "Music Command".
3. Add the "Ask for Text" action. For the prompt, you can put "What do you want to play?".
4. Add the "Run Shell Script" action.
5. In the script box, type: /usr/local/bin/dj "$Provided_Input"
   - Make sure "Pass Input" is set to "To stdin".
   - The "$Provided_Input" is a magic variable representing the text from the previous step.
6. (Optional) Add a "Show Result" action to display the text output from the script.

--- How to Use with Siri ---
Say "Hey Siri, Music Command".
Siri will ask "What do you want to play?".
Respond with your command, for example: "Play songs by The Beatles from 1967".
The shortcut will run, your Go program will play the music, and the `say` command will provide voice feedback.
*/
