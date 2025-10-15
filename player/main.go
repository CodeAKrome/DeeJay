package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dhowden/tag"
	"github.com/schollz/progressbar/v3"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	dbName         = "music"
	collectionName = "songs"
	defaultWorkers = 8
)

// Song represents the structure of a music file's metadata in MongoDB.
type Song struct {
	ID          string    `bson:"_id"` // File path
	Title       string    `bson:"title"`
	Artist      string    `bson:"artist"`
	Album       string    `bson:"album"`
	AlbumArtist string    `bson:"album_artist"`
	Composer    string    `bson:"composer"`
	Genre       string    `bson:"genre"`
	Year        int       `bson:"year"`
	TrackNum    int       `bson:"track_num"`
	TrackTotal  int       `bson:"track_total"`
	DiscNum     int       `bson:"disc_num"`
	DiscTotal   int       `bson:"disc_total"`
	Lyrics      string    `bson:"lyrics,omitempty"`
	Comment     string    `bson:"comment,omitempty"`
	Format      string    `bson:"format"`
	FileSize    int64     `bson:"file_size"`
	FileHash    string    `bson:"file_hash"` // SHA256 hash of the file content
	IndexedAt   time.Time `bson:"indexed_at"`
}

// QueryCriteria represents parsed search criteria
type QueryCriteria struct {
	Genre     string
	Artist    string
	Album     string
	Title     string
	YearStart int
	YearEnd   int
	Decade    int
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage:")
		fmt.Printf("  Index mode:  %s index <music_directory>\n", os.Args[0])
		fmt.Printf("  Query mode:  %s query\n", os.Args[0])
		os.Exit(1)
	}

	mode := os.Args[1]

	switch mode {
	case "index":
		if len(os.Args) < 3 {
			log.Fatalf("Usage: %s index <music_directory>", os.Args[0])
		}
		indexMusic(os.Args[2])
	case "query":
		queryMusic()
	default:
		log.Fatalf("Unknown mode: %s. Use 'index' or 'query'", mode)
	}
}

func indexMusic(musicDir string) {
	// --- MongoDB Connection ---
	collection := connectToMongo()

	// --- File Discovery ---
	log.Printf("Scanning for music files in %s...", musicDir)
	var filePaths []string
	err := filepath.Walk(musicDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && isMusicFile(path) {
			filePaths = append(filePaths, path)
		}
		return nil
	})
	if err != nil {
		log.Fatalf("Error walking directory: %v", err)
	}

	if len(filePaths) == 0 {
		log.Println("No music files found.")
		return
	}
	log.Printf("Found %d music files to process.", len(filePaths))

	// --- Parallel Processing Setup ---
	numWorkers := runtime.NumCPU()
	if numWorkers > defaultWorkers {
		numWorkers = defaultWorkers
	}
	jobs := make(chan string, len(filePaths))
	results := make(chan *Song, len(filePaths))
	var wg sync.WaitGroup

	for w := 1; w <= numWorkers; w++ {
		wg.Add(1)
		go worker(w, jobs, results, &wg)
	}

	for _, path := range filePaths {
		jobs <- path
	}
	close(jobs)

	// --- Progress Bar and Result Collection ---
	bar := progressbar.NewOptions(len(filePaths),
		progressbar.OptionSetDescription("Processing files"),
		progressbar.OptionSetWidth(40),
		progressbar.OptionShowCount(),
		progressbar.OptionEnableColorCodes(true),
	)

	var songsToInsert []interface{}
	var processedCount int
	done := make(chan struct{})
	go func() {
		for song := range results {
			if song != nil {
				songsToInsert = append(songsToInsert, song)
			}
			processedCount++
			bar.Add(1)
			if processedCount == len(filePaths) {
				close(done)
				return
			}
		}
	}()

	wg.Wait()
	close(results)
	<-done // Wait for result collection to finish

	// --- Bulk Insert into MongoDB ---
	if len(songsToInsert) > 0 {
		log.Printf("\nInserting %d songs into MongoDB...", len(songsToInsert))
		_, err := collection.DeleteMany(context.Background(), bson.D{}) // Clear collection before new import
		if err != nil {
			log.Fatalf("Failed to clear collection: %v", err)
		}
		_, err = collection.InsertMany(context.Background(), songsToInsert)
		if err != nil {
			log.Fatalf("Failed to insert songs: %v", err)
		}
		log.Println("Successfully inserted songs.")
	}

	// --- Create Indexes ---
	log.Println("Creating database indexes...")
	if err := createIndexes(collection); err != nil {
		log.Fatalf("Failed to create indexes: %v", err)
	}
	log.Println("Indexes created successfully.")

	// --- Find and Report Duplicates ---
	log.Println("Finding duplicate files...")
	if err := findAndReportDuplicates(collection); err != nil {
		log.Fatalf("Failed to find duplicates: %v", err)
	}
}

func queryMusic() {
	collection := connectToMongo()

	fmt.Println("\n=== DeeJay Music Query ===")
	fmt.Println("Enter queries like:")
	fmt.Println("  - play punk from the 1980s")
	fmt.Println("  - play rock from 1995 to 2000")
	fmt.Println("  - play jazz by Miles Davis")
	fmt.Println("  - play songs from the 70s")
	fmt.Println("Type 'quit' to exit\n")

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}

		query := strings.TrimSpace(scanner.Text())
		if query == "" {
			continue
		}
		if strings.ToLower(query) == "quit" || strings.ToLower(query) == "exit" {
			break
		}

		criteria := parseQuery(query)
		songs, err := searchSongs(collection, criteria)
		if err != nil {
			fmt.Printf("Error searching: %v\n", err)
			continue
		}

		if len(songs) == 0 {
			fmt.Println("No songs found matching your criteria.")
			continue
		}

		fmt.Printf("\nFound %d songs:\n", len(songs))
		for i, song := range songs {
			fmt.Printf("%d. %s - %s (%d) [%s]\n", i+1, song.Artist, song.Title, song.Year, song.Genre)
		}

		fmt.Printf("\nPlay all? (y/n): ")
		if !scanner.Scan() {
			break
		}
		if strings.ToLower(strings.TrimSpace(scanner.Text())) == "y" {
			playSongs(songs)
		}
		fmt.Println()
	}
}

func connectToMongo() *mongo.Collection {
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
		log.Fatalf("Failed to connect to MongoDB: %v", err)
	}

	log.Println("Successfully connected to MongoDB.")
	return client.Database(dbName).Collection(collectionName)
}

// parseQuery extracts search criteria from natural language queries
func parseQuery(query string) QueryCriteria {
	query = strings.ToLower(query)
	criteria := QueryCriteria{}

	// Extract genre (after "play" and before "from/by")
	genrePattern := regexp.MustCompile(`play\s+(\w+(?:\s+\w+)?)\s+(?:from|by|in)`)
	if matches := genrePattern.FindStringSubmatch(query); len(matches) > 1 {
		criteria.Genre = strings.TrimSpace(matches[1])
	}

	// Extract artist (after "by")
	artistPattern := regexp.MustCompile(`by\s+([a-z0-9\s]+?)(?:\s+from|\s+in|$)`)
	if matches := artistPattern.FindStringSubmatch(query); len(matches) > 1 {
		criteria.Artist = strings.TrimSpace(matches[1])
	}

	// Extract decade patterns (1980s, 80s, the 80s, etc.)
	decadePattern := regexp.MustCompile(`(?:the\s+)?(?:19)?([0-9])0s`)
	if matches := decadePattern.FindStringSubmatch(query); len(matches) > 1 {
		decade, _ := strconv.Atoi(matches[1])
		if decade >= 5 && decade <= 9 {
			criteria.Decade = 1900 + decade*10
		} else if decade >= 0 && decade <= 2 {
			criteria.Decade = 2000 + decade*10
		}
		criteria.YearStart = criteria.Decade
		criteria.YearEnd = criteria.Decade + 9
	}

	// Extract year range (1995 to 2000, 1995-2000)
	yearRangePattern := regexp.MustCompile(`(\d{4})\s+(?:to|-)\s+(\d{4})`)
	if matches := yearRangePattern.FindStringSubmatch(query); len(matches) > 2 {
		criteria.YearStart, _ = strconv.Atoi(matches[1])
		criteria.YearEnd, _ = strconv.Atoi(matches[2])
	}

	// Extract single year (in 1995, from 1995)
	if criteria.YearStart == 0 {
		yearPattern := regexp.MustCompile(`(?:in|from)\s+(\d{4})`)
		if matches := yearPattern.FindStringSubmatch(query); len(matches) > 1 {
			year, _ := strconv.Atoi(matches[1])
			criteria.YearStart = year
			criteria.YearEnd = year
		}
	}

	return criteria
}

// searchSongs queries MongoDB based on the parsed criteria
func searchSongs(collection *mongo.Collection, criteria QueryCriteria) ([]Song, error) {
	filter := bson.M{}

	if criteria.Genre != "" {
		filter["genre"] = bson.M{"$regex": criteria.Genre, "$options": "i"}
	}

	if criteria.Artist != "" {
		filter["artist"] = bson.M{"$regex": criteria.Artist, "$options": "i"}
	}

	if criteria.YearStart > 0 && criteria.YearEnd > 0 {
		filter["year"] = bson.M{
			"$gte": criteria.YearStart,
			"$lte": criteria.YearEnd,
		}
	}

	cursor, err := collection.Find(context.Background(), filter)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(context.Background())

	var songs []Song
	if err := cursor.All(context.Background(), &songs); err != nil {
		return nil, err
	}

	return songs, nil
}

// playSongs attempts to play the songs using the system's default player
func playSongs(songs []Song) {
	for _, song := range songs {
		fmt.Printf("Playing: %s - %s\n", song.Artist, song.Title)

		// Try to find an available media player
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin": // macOS
			cmd = exec.Command("afplay", song.ID)
		case "linux":
			// Try common Linux players
			if _, err := exec.LookPath("mpg123"); err == nil {
				cmd = exec.Command("mpg123", song.ID)
			} else if _, err := exec.LookPath("ffplay"); err == nil {
				cmd = exec.Command("ffplay", "-nodisp", "-autoexit", song.ID)
			}
		case "windows":
			cmd = exec.Command("cmd", "/C", "start", song.ID)
		}

		if cmd != nil {
			if err := cmd.Run(); err != nil {
				fmt.Printf("Error playing %s: %v\n", song.ID, err)
			}
		} else {
			fmt.Println("No suitable media player found. Install mpg123 or ffplay.")
			return
		}
	}
}

// worker processes files from the jobs channel and sends results to the results channel.
func worker(id int, jobs <-chan string, results chan<- *Song, wg *sync.WaitGroup) {
	defer wg.Done()
	for path := range jobs {
		file, err := os.Open(path)
		if err != nil {
			log.Printf("Worker %d: Error opening %s: %v", id, path, err)
			results <- nil
			continue
		}

		meta, err := tag.ReadFrom(file)
		if err != nil {
			log.Printf("Worker %d: Error reading tags from %s: %v", id, path, err)
			file.Close()
			results <- nil
			continue
		}

		// Reset file pointer to calculate hash from the beginning
		file.Seek(0, 0)
		hash, fileSize, err := calculateHashAndSize(file)
		if err != nil {
			log.Printf("Worker %d: Error hashing %s: %v", id, path, err)
			file.Close()
			results <- nil
			continue
		}
		file.Close()

		trackNum, trackTotal := meta.Track()
		discNum, discTotal := meta.Disc()

		song := &Song{
			ID:          path,
			Title:       meta.Title(),
			Artist:      meta.Artist(),
			Album:       meta.Album(),
			AlbumArtist: meta.AlbumArtist(),
			Composer:    meta.Composer(),
			Genre:       meta.Genre(),
			Year:        meta.Year(),
			TrackNum:    trackNum,
			TrackTotal:  trackTotal,
			DiscNum:     discNum,
			DiscTotal:   discTotal,
			Lyrics:      meta.Lyrics(),
			Comment:     meta.Comment(),
			Format:      string(meta.Format()),
			FileSize:    fileSize,
			FileHash:    hash,
			IndexedAt:   time.Now().UTC(),
		}
		results <- song
	}
}

// isMusicFile checks if a file has a common music file extension.
func isMusicFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp3", ".m4a", ".flac", ".ogg", ".wav", ".aac":
		return true
	default:
		return false
	}
}

// calculateHashAndSize computes the SHA256 hash and size of a file.
func calculateHashAndSize(r io.Reader) (string, int64, error) {
	hasher := sha256.New()
	size, err := io.Copy(hasher, r)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), size, nil
}

// createIndexes sets up the necessary indexes in the MongoDB collection for efficient searching.
func createIndexes(collection *mongo.Collection) error {
	models := []mongo.IndexModel{
		{Keys: bson.D{{Key: "artist", Value: 1}}},
		{Keys: bson.D{{Key: "title", Value: 1}}},
		{Keys: bson.D{{Key: "year", Value: 1}}},
		{Keys: bson.D{{Key: "genre", Value: 1}}},
		{Keys: bson.D{{Key: "file_hash", Value: 1}}}, // For finding duplicates
	}
	_, err := collection.Indexes().CreateMany(context.Background(), models)
	return err
}

// findAndReportDuplicates queries MongoDB for files with the same hash and writes them to a TSV file.
func findAndReportDuplicates(collection *mongo.Collection) error {
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$file_hash"},
			{Key: "paths", Value: bson.D{{Key: "$push", Value: "$_id"}}},
			{Key: "sizes", Value: bson.D{{Key: "$push", Value: "$file_size"}}},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "count", Value: bson.D{{Key: "$gt", Value: 1}}},
		}}},
	}

	cursor, err := collection.Aggregate(context.Background(), pipeline)
	if err != nil {
		return fmt.Errorf("aggregation failed: %w", err)
	}
	defer cursor.Close(context.Background())

	outFile, err := os.Create("duplicates.tsv")
	if err != nil {
		return fmt.Errorf("failed to create duplicates.tsv: %w", err)
	}
	defer outFile.Close()

	// Write TSV header
	_, err = outFile.WriteString("file_hash\tfile_size\tfile_path\n")
	if err != nil {
		return err
	}

	foundDuplicates := false
	for cursor.Next(context.Background()) {
		foundDuplicates = true
		var result struct {
			Hash  string   `bson:"_id"`
			Paths []string `bson:"paths"`
			Sizes []int64  `bson:"sizes"`
		}
		if err := cursor.Decode(&result); err != nil {
			log.Printf("Error decoding duplicate result: %v", err)
			continue
		}

		for i, path := range result.Paths {
			line := fmt.Sprintf("%s\t%d\t%s\n", result.Hash, result.Sizes[i], path)
			if _, err := outFile.WriteString(line); err != nil {
				log.Printf("Error writing to duplicates.tsv: %v", err)
			}
		}
	}

	if !foundDuplicates {
		log.Println("No duplicate files found.")
		// Clean up empty file
		outFile.Close()
		os.Remove("duplicates.tsv")
	} else {
		log.Println("Duplicate file report saved to duplicates.tsv")
	}

	return cursor.Err()
}
