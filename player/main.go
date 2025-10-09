package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	dbName         = "music"
	collectionName = "songs"
)

// Song represents the structure of a music file's metadata in MongoDB.
// This should be identical to the one in your indexer.
type Song struct {
	ID     string `bson:"_id"` // File path
	Title  string `bson:"title"`
	Artist string `bson:"artist"`
	Album  string `bson:"album"`
	Year   int    `bson:"year"`
}

func main() {
	if len(os.Args) < 2 {
		// Using `say` for voice feedback on error
		speak("You need to provide a command.")
		log.Fatalf("Usage: %s \"<command>\"", os.Args[0])
	}
	command := os.Args[1]

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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		speak("I could not connect to the music database.")
		log.Fatalf("Failed to connect to MongoDB: %v", err)
	}
	defer client.Disconnect(context.Background())

	collection := client.Database(dbName).Collection(collectionName)

	// --- Parse Command and Execute ---
	filter := parseCommand(command)
	if len(filter) == 0 {
		speak("Sorry, I didn't understand that. Please try something like, play songs by Queen, or play music from 1999.")
		return
	}

	findOptions := options.Find()
	findOptions.SetSort(bson.D{{Key: "artist", Value: 1}, {Key: "year", Value: 1}, {Key: "album", Value: 1}, {Key: "track_num", Value: 1}})

	cursor, err := collection.Find(context.Background(), filter, findOptions)
	if err != nil {
		speak("I had trouble searching for that music.")
		log.Fatalf("Failed to query MongoDB: %v", err)
	}
	defer cursor.Close(context.Background())

	var songs []Song
	if err = cursor.All(context.Background(), &songs); err != nil {
		speak("I had trouble getting the song list.")
		log.Fatalf("Failed to decode songs: %v", err)
	}

	if len(songs) == 0 {
		speak("I couldn't find any music matching your request.")
		return
	}

	// --- Play Music ---
	playSongs(songs)
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

	for i, song := range songs {
		log.Printf("Playing (%d/%d): %s - %s", i+1, count, song.Artist, song.Title)
		cmd := exec.Command("afplay", song.ID)
		// This will block until the song is finished.
		// For a more advanced player, you'd run this in a goroutine
		// and add controls for skip, stop, etc.
		err := cmd.Run()
		if err != nil {
			log.Printf("Error playing %s: %v", song.ID, err)
		}
	}
	log.Println("Playlist finished.")
}

// parseCommand is a simple NLU to convert a text command into a MongoDB filter.
func parseCommand(command string) bson.M {
	filter := bson.M{}
	command = strings.ToLower(command)

	// --- Keyword: by (Artist) ---
	// Example: "play songs by Queen" or "play songs by "The Beatles""
	if matches := findNamedCapture(command, `by (?:"([^"]+)"|(\w+(?:\s\w+)*))`); len(matches) > 0 {
		filter["artist"] = bson.M{"$regex": strings.Join(matches, ""), "$options": "i"}
	}

	// --- Keyword: called / title (Song Title) ---
	// Example: "play the song called "Hey Jude""
	if matches := findNamedCapture(command, `(?:called|title) (?:"([^"]+)"|(\w+(?:\s\w+)*))`); len(matches) > 0 {
		filter["title"] = bson.M{"$regex": strings.Join(matches, ""), "$options": "i"}
	}

	// --- Keyword: from the album / album ---
	// Example: "play songs from the album "A Night at the Opera""
	if matches := findNamedCapture(command, `(?:from the album|album) (?:"([^"]+)"|(\w+(?:\s\w+)*))`); len(matches) > 0 {
		filter["album"] = bson.M{"$regex": strings.Join(matches, ""), "$options": "i"}
	}

	// --- Keyword: from the year / in (Year) ---
	// Example: "play music from the year 1999" or "play music in 1999"
	yearRe := regexp.MustCompile(`(?:from the year|in|from) (\d{4})`)
	if yearMatch := yearRe.FindStringSubmatch(command); len(yearMatch) > 1 {
		if year, err := strconv.Atoi(yearMatch[1]); err == nil {
			filter["year"] = year
		}
	}

	// If we are just playing a single song, be more specific with the title search
	if strings.Contains(command, "play the song") && filter["title"] != nil {
		// Let's try an exact match first for single songs
		if titlePattern, ok := filter["title"].(bson.M)["$regex"].(string); ok {
			filter["title"] = bson.M{"$regex": fmt.Sprintf("^%s$", titlePattern), "$options": "i"}
		}
	}

	return filter
}

// findNamedCapture is a helper to extract a value that is either in quotes or is a series of words.
func findNamedCapture(text, pattern string) []string {
	r := regexp.MustCompile(pattern)
	matches := r.FindStringSubmatch(text)

	if len(matches) > 1 {
		// The result will be in one of the capture groups.
		// The first group is the full match, so we check from index 1 onwards.
		for i := 1; i < len(matches); i++ {
			if matches[i] != "" {
				return []string{matches[i]}
			}
		}
	}
	return nil
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
