package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
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

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "index":
		if len(os.Args) < 3 {
			log.Fatalf("Usage: %s index <music_directory>", os.Args[0])
		}
		musicDir := os.Args[2]
		runIndexer(musicDir)
	default:
		log.Printf("Unknown command: %s", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func runIndexer(musicDir string) {
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
		log.Fatalf("Failed to connect to MongoDB: %v", err)
	}
	defer client.Disconnect(context.Background())

	log.Println("Successfully connected to MongoDB.")
	collection := client.Database(dbName).Collection(collectionName)

	// --- File Discovery ---
	log.Printf("Scanning for music files in %s...", musicDir)
	var filePaths []string
	err = filepath.Walk(musicDir, func(path string, info os.FileInfo, err error) error {
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

func printUsage() {
	fmt.Printf("Usage: %s <command> [arguments]\n\n", os.Args[0])
	fmt.Println("Available commands:")
	fmt.Println("  index <music_directory>    Scans a directory and indexes music files into MongoDB.")
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
