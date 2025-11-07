package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	pidFile        = "/tmp/deejay.pid"
	commandFile    = "/tmp/deejay.cmd"
)

type Song struct {
	ID          string    `bson:"_id"`
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
	FileHash    string    `bson:"file_hash"`
	IndexedAt   time.Time `bson:"indexed_at"`
}

type QueryCriteria struct {
	Genre     string
	Artist    string
	Album     string
	Title     string
	YearStart int
	YearEnd   int
	Decade    int
}

func printHelp() {
	fmt.Printf("Usage: %s <command> [options]\n\n", os.Args[0])
	fmt.Println("Commands:")
	fmt.Println("  index <music_directory>   Scan a directory and index music files into the database.")
	fmt.Println("  query [\"query string\"]    Start interactive query mode or run a single query.")
	fmt.Println("  skip                      Skip the currently playing song.")
	fmt.Println("  stop                      Stop playback and exit the player.")
	fmt.Println("  help                      Show this help message.")
	fmt.Println("\nOptions for 'query':")
	fmt.Println("  --limit <number>          Limit the number of songs in the playlist.")
	fmt.Println("  -r, -random               Randomize the playlist before playing.")
	fmt.Println("\nExamples:")
	fmt.Printf("  %s index /path/to/my/music\n", os.Args[0])
	fmt.Printf("  %s query\n", os.Args[0])
	fmt.Printf("  %s query \"play rock from the 90s\"\n", os.Args[0])
	fmt.Printf("  %s -r query \"play jazz by miles davis\"\n", os.Args[0])
	fmt.Printf("  %s skip\n", os.Args[0])
}

func main() {
	if len(os.Args) < 2 {
		printHelp()
		os.Exit(1)
	}

	randomize := false
	limit := 0
	rawArgs := os.Args[1:]
	args := make([]string, 0, len(rawArgs))

	// Manual flag parsing to handle flags before the command
	i := 0
	for i < len(rawArgs) {
		arg := rawArgs[i]
		if arg == "-r" || arg == "-random" {
			randomize = true
			i++
		} else if arg == "--limit" {
			if i+1 < len(rawArgs) {
				if val, err := strconv.Atoi(rawArgs[i+1]); err == nil {
					limit = val
				}
				i++
			}
			i++
		} else {
			args = append(args, rawArgs[i:]...)
			break
		}
	}

	// Seed the random number generator if we're randomizing.
	if randomize {
		rand.Seed(time.Now().UnixNano())
	}
	if len(args) == 0 {
		printHelp()
		os.Exit(1)
	}

	mode := args[0]
	if mode == "help" || mode == "-h" || mode == "--help" {
		printHelp()
		return
	}

	switch args[0] {
	case "index":
		if len(args) < 2 {
			log.Fatalf("Usage: %s index <music_directory>", os.Args[0])
		}
		indexMusic(args[1])
	case "query":
		cliQuery := ""
		if len(args) > 1 {
			cliQuery = strings.Join(args[1:], " ")
		}
		queryMusic(cliQuery, randomize, limit)
	case "skip":
		sendCommand("skip")
	case "stop":
		sendCommand("stop")
	default:
		log.Fatalf("Unknown mode: %s", args[0])
	}
}

/* ---------- indexing ---------- */
func indexMusic(musicDir string) {
	collection := connectToMongo()

	log.Printf("Scanning for music files in %s...", musicDir)
	var filePaths []string
	_ = filepath.Walk(musicDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && isMusicFile(path) {
			filePaths = append(filePaths, path)
		}
		return nil
	})
	if len(filePaths) == 0 {
		log.Println("No music files found.")
		return
	}
	log.Printf("Found %d music files to process.", len(filePaths))

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
	for _, p := range filePaths {
		jobs <- p
	}
	close(jobs)

	bar := progressbar.NewOptions(len(filePaths),
		progressbar.OptionSetDescription("Processing files"),
		progressbar.OptionSetWidth(40),
		progressbar.OptionShowCount(),
		progressbar.OptionEnableColorCodes(true))

	var songsToInsert []interface{}
	processed := 0
	done := make(chan struct{})
	go func() {
		for s := range results {
			if s != nil {
				songsToInsert = append(songsToInsert, s)
			}
			processed++
			bar.Add(1)
			if processed == len(filePaths) {
				close(done)
				return
			}
		}
	}()
	wg.Wait()
	close(results)
	<-done

	if len(songsToInsert) > 0 {
		log.Printf("\nInserting %d songs into MongoDB...", len(songsToInsert))
		_, _ = collection.DeleteMany(context.Background(), bson.D{})
		_, _ = collection.InsertMany(context.Background(), songsToInsert)
		log.Println("Successfully inserted songs.")
	}

	_ = createIndexes(collection)
	_ = findAndReportDuplicates(collection)
}

/* ---------- query / playback ---------- */
func queryMusic(cliQuery string, randomize bool, limit int) {
	collection := connectToMongo()

	if err := ioutil.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
		log.Printf("Warning: Could not write PID file: %v", err)
	}
	defer os.Remove(pidFile)
	defer os.Remove(commandFile)

	if cliQuery == "" {
		fmt.Println("\n=== DeeJay Music Query ===")
		fmt.Println("Enter queries like:")
		fmt.Println("  - play punk from the 1980s")
		fmt.Println("  - play rock from 1995 to 2000")
		fmt.Println("  - play jazz by Miles Davis")
		fmt.Println("  - play songs from the 70s")
		fmt.Println("Type 'quit' to exit\n")
	}

	scanner := bufio.NewScanner(os.Stdin)
	for {
		var query string
		if cliQuery != "" {
			query = cliQuery
			cliQuery = ""
		} else {
			fmt.Print("> ")
			if !scanner.Scan() {
				break
			}
			query = strings.TrimSpace(scanner.Text())
		}
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
		if randomize {
			rand.Shuffle(len(songs), func(i, j int) { songs[i], songs[j] = songs[j], songs[i] })
		}
		if limit > 0 && len(songs) > limit {
			songs = songs[:limit]
		}

		// Summarize playlist and decide whether to ask for confirmation
		summarizePlaylist(songs)

		// Skip confirmation if -r is used or if it's a direct CLI query
		shouldPlay := randomize || cliQuery != ""

		if !shouldPlay {
			fmt.Printf("\nPlay all? (y/n): ")
			if !scanner.Scan() {
				break
			}
			if strings.ToLower(strings.TrimSpace(scanner.Text())) == "y" {
				shouldPlay = true
			} else {
				continue
			}
		}
		playSongs(songs)
		if cliQuery == "" {
			fmt.Println()
		} else {
			break
		}
	}
}

func summarizePlaylist(songs []Song) {
	if len(songs) == 0 {
		return
	}

	artistCounts := make(map[string]int)
	genreCounts := make(map[string]int)

	for _, s := range songs {
		artistCounts[s.Artist]++
		if s.Genre != "" {
			genreCounts[s.Genre]++
		}
	}

	fmt.Printf("\nFound %d songs.\n", len(songs))
	fmt.Println("--- Artists ---")
	for artist, count := range artistCounts {
		fmt.Printf("  %s: %d\n", artist, count)
	}
	fmt.Println("--- Genres ---")
	for genre, count := range genreCounts {
		fmt.Printf("  %s: %d\n", genre, count)
	}
	fmt.Println("---------------")
}

/* ---------- mongo ---------- */
func connectToMongo() *mongo.Collection {
	user := os.Getenv("MONGO_USER")
	if user == "" {
		user = "root"
	}
	pass := os.Getenv("MONGO_PASS")
	if pass == "" {
		pass = "example"
	}
	uri := fmt.Sprintf("mongodb://%s:%s@localhost:27017", user, pass)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		log.Fatalf("Failed to connect to MongoDB: %v", err)
	}
	log.Println("Successfully connected to MongoDB.")
	return client.Database(dbName).Collection(collectionName)
}

/* ---------- query parsing ---------- */
func parseQuery(q string) QueryCriteria {
	q = strings.ToLower(q)
	c := QueryCriteria{}

	if m := regexp.MustCompile(`play\s+(\w+(?:\s+\w+)?)\s+(?:from|by|in)`).FindStringSubmatch(q); len(m) > 1 {
		c.Genre = strings.TrimSpace(m[1])
	}
	if m := regexp.MustCompile(`by\s+([a-z0-9\s]+?)(?:\s+from|\s+in|$)`).FindStringSubmatch(q); len(m) > 1 {
		c.Artist = strings.TrimSpace(m[1])
	}
	if m := regexp.MustCompile(`(?:the\s+)?(?:19)?([0-9])0s`).FindStringSubmatch(q); len(m) > 1 {
		decade, _ := strconv.Atoi(m[1])
		if decade >= 5 && decade <= 9 {
			c.Decade = 1900 + decade*10
		} else if decade >= 0 && decade <= 2 {
			c.Decade = 2000 + decade*10
		}
		c.YearStart, c.YearEnd = c.Decade, c.Decade+9
	}
	if m := regexp.MustCompile(`(\d{4})\s+(?:to|-)\s+(\d{4})`).FindStringSubmatch(q); len(m) > 2 {
		c.YearStart, _ = strconv.Atoi(m[1])
		c.YearEnd, _ = strconv.Atoi(m[2])
	}
	if c.YearStart == 0 {
		if m := regexp.MustCompile(`(?:in|from)\s+(\d{4})`).FindStringSubmatch(q); len(m) > 1 {
			yr, _ := strconv.Atoi(m[1])
			c.YearStart, c.YearEnd = yr, yr
		}
	}
	return c
}

/* ---------- search ---------- */
func searchSongs(coll *mongo.Collection, criteria QueryCriteria) ([]Song, error) {
	filter := bson.M{}
	if criteria.Genre != "" {
		filter["genre"] = bson.M{"$regex": criteria.Genre, "$options": "i"}
	}
	if criteria.Artist != "" {
		filter["artist"] = bson.M{"$regex": criteria.Artist, "$options": "i"}
	}
	if criteria.YearStart > 0 && criteria.YearEnd > 0 {
		filter["year"] = bson.M{"$gte": criteria.YearStart, "$lte": criteria.YearEnd}
	}
	cur, err := coll.Find(context.Background(), filter)
	if err != nil {
		return nil, err
	}
	defer cur.Close(context.Background())
	var songs []Song
	_ = cur.All(context.Background(), &songs)
	return songs, nil
}

/* ---------- control commands ---------- */
func sendCommand(cmd string) {
	if _, err := os.Stat(pidFile); os.IsNotExist(err) {
		fmt.Println("DeeJay is not running in query mode.")
		os.Exit(1)
	}
	_ = ioutil.WriteFile(commandFile, []byte(cmd), 0644)
	data, _ := ioutil.ReadFile(pidFile)
	var pid int
	fmt.Sscanf(string(data), "%d", &pid)
	p, _ := os.FindProcess(pid)
	_ = p.Signal(syscall.SIGUSR1)
	fmt.Printf("Sent '%s' command to DeeJay\n", cmd)
}

/* ---------- playback ---------- */
func playSongs(songs []Song) {
	fmt.Println("\nControls: Press 's' to skip, 'q' to stop playback")
	fmt.Println("Or use './dj skip' or './dj stop' from another terminal")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGUSR1)
	inputChan := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			inputChan <- strings.ToLower(strings.TrimSpace(sc.Text()))
		}
	}()
	commandChan := make(chan string, 1)
	go func() {
		for range sigChan {
			if b, err := ioutil.ReadFile(commandFile); err == nil {
				commandChan <- string(b)
				_ = os.Remove(commandFile)
			}
		}
	}()

	for i, song := range songs {
		fmt.Printf("\n[%d/%d] Playing: %s - %s", i+1, len(songs), song.Artist, song.Title)

		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.Command("afplay", song.ID)
		case "linux":
			if _, err := exec.LookPath("mpg123"); err == nil {
				cmd = exec.Command("mpg123", "-q", song.ID)
			} else if _, err := exec.LookPath("ffplay"); err == nil {
				cmd = exec.Command("ffplay", "-nodisp", "-autoexit", song.ID)
			}
		case "windows":
			cmd = exec.Command("cmd", "/C", "start", "/wait", song.ID)
		}
		if cmd == nil {
			fmt.Println("No suitable media player found. Install mpg123 or ffplay.")
			return
		}
		_ = cmd.Start()

		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		stopped := false
		select {
		case in := <-inputChan:
			switch in {
			case "s", "skip":
				fmt.Println("⏭️  Skipping to next song...")
				_ = cmd.Process.Kill()
			case "q", "quit", "stop":
				fmt.Println("⏹️  Stopping playback...")
				_ = cmd.Process.Kill()
				stopped = true
			}
		case cmdIn := <-commandChan:
			switch cmdIn {
			case "skip":
				fmt.Println("⏭️  Skipping to next song (external command)...")
				_ = cmd.Process.Kill()
			case "stop":
				fmt.Println("⏹️  Stopping playback (external command)...")
				_ = cmd.Process.Kill()
				stopped = true
			}
		case <-done:
		}
		if stopped {
			break
		}
	}
	signal.Stop(sigChan)
	fmt.Println("\n✓ Playback finished")
}

/* ---------- workers / helpers ---------- */
func worker(id int, jobs <-chan string, results chan<- *Song, wg *sync.WaitGroup) {
	defer wg.Done()
	for path := range jobs {
		f, err := os.Open(path)
		if err != nil {
			results <- nil
			continue
		}
		meta, err := tag.ReadFrom(f)
		if err != nil {
			f.Close()
			results <- nil
			continue
		}
		_, _ = f.Seek(0, 0)
		hash, size, err := calculateHashAndSize(f)
		f.Close()
		if err != nil {
			results <- nil
			continue
		}
		track, trackTotal := meta.Track()
		disc, discTotal := meta.Disc()
		results <- &Song{
			ID: path, Title: meta.Title(), Artist: meta.Artist(), Album: meta.Album(),
			AlbumArtist: meta.AlbumArtist(), Composer: meta.Composer(), Genre: meta.Genre(),
			Year: meta.Year(), TrackNum: track, TrackTotal: trackTotal,
			DiscNum: disc, DiscTotal: discTotal, Lyrics: meta.Lyrics(), Comment: meta.Comment(),
			Format: string(meta.Format()), FileSize: size, FileHash: hash, IndexedAt: time.Now().UTC(),
		}
	}
}

func isMusicFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp3", ".m4a", ".flac", ".ogg", ".wav", ".aac":
		return true
	}
	return false
}

func calculateHashAndSize(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

/* ---------- mongo indexes ---------- */
func createIndexes(coll *mongo.Collection) error {
	models := []mongo.IndexModel{
		{Keys: bson.D{{Key: "artist", Value: 1}}},
		{Keys: bson.D{{Key: "title", Value: 1}}},
		{Keys: bson.D{{Key: "year", Value: 1}}},
		{Keys: bson.D{{Key: "genre", Value: 1}}},
		{Keys: bson.D{{Key: "file_hash", Value: 1}}},
	}
	_, err := coll.Indexes().CreateMany(context.Background(), models)
	return err
}

/* ---------- duplicates report ---------- */
func findAndReportDuplicates(coll *mongo.Collection) error {
	pipe := mongo.Pipeline{
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$file_hash"},
			{Key: "paths", Value: bson.D{{Key: "$push", Value: "$_id"}}},
			{Key: "sizes", Value: bson.D{{Key: "$push", Value: "$file_size"}}},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		bson.D{{Key: "$match", Value: bson.D{{Key: "count", Value: bson.D{{Key: "$gt", Value: 1}}}}}},
	}
	cur, _ := coll.Aggregate(context.Background(), pipe)
	defer cur.Close(context.Background())

	out, _ := os.Create("duplicates.tsv")
	defer out.Close()
	_, _ = out.WriteString("file_hash\tfile_size\tfile_path\n")

	found := false
	for cur.Next(context.Background()) {
		found = true
		var res struct {
			Hash  string   `bson:"_id"`
			Paths []string `bson:"paths"`
			Sizes []int64  `bson:"sizes"`
		}
		_ = cur.Decode(&res)
		for i, p := range res.Paths {
			_, _ = fmt.Fprintf(out, "%s\t%d\t%s\n", res.Hash, res.Sizes[i], p)
		}
	}
	if !found {
		_ = os.Remove("duplicates.tsv")
		log.Println("No duplicate files found.")
	} else {
		log.Println("Duplicate file report saved to duplicates.tsv")
	}
	return cur.Err()
}
