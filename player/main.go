package main

import (
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dhowden/tag"
	"github.com/nsf/termbox-go"
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

type TUIState struct {
	query        string
	cursorPos    int
	activeWindow string // "query", "artist", "genre", "year"
	artistScroll int
	genreScroll  int
	yearScroll   int
	artists      []CountItem
	genres       []CountItem
	years        []CountItem
	currentSong  *Song
	songIndex    int
	totalSongs   int
	playlist     []Song // Full playlist for showing upcoming songs
	message      string
	showHelp     bool
}

type CountItem struct {
	Key   string
	Value int
}

func printHelp() {
	fmt.Printf("Usage: %s <command> [options]\n\n", os.Args[0])
	fmt.Println("Commands:")
	fmt.Println("  index <music_directory>   Scan a directory and index music files into the database.")
	fmt.Println("  query [\"query string\"]    Start interactive query mode with TUI.")
	fmt.Println("  list                      List all songs in the database with summary statistics.")
	fmt.Println("  skip                      Skip the currently playing song.")
	fmt.Println("  stop                      Stop playback and exit the player.")
	fmt.Println("  help                      Show this help message.")
	fmt.Println("\nOptions for 'query':")
	fmt.Println("  --limit <number>          Limit the number of songs in the playlist.")
	fmt.Println("  -r, -random               Randomize the playlist before playing.")
	fmt.Println("\nOptions for 'list':")
	fmt.Println("  --start-year <year>       Only list songs from this year onwards.")
	fmt.Println("  --end-year <year>         Only list songs up to and including this year.")
	fmt.Println("\nExamples:")
	fmt.Printf("  %s index /path/to/my/music\n", os.Args[0])
	fmt.Printf("  %s query\n", os.Args[0])
	fmt.Printf("  %s query \"play rock from the 90s\"\n", os.Args[0])
	fmt.Printf("  %s -r query \"play jazz by miles davis\"\n", os.Args[0])
	fmt.Printf("  %s list\n", os.Args[0])
	fmt.Printf("  %s skip\n", os.Args[0])
}

func main() {
	if len(os.Args) < 2 {
		printHelp()
		os.Exit(1)
	}

	randomize := false
	limit := 0
	startYear := 0
	endYear := 0
	rawArgs := os.Args[1:]
	args := make([]string, 0, len(rawArgs))

	for i := 0; i < len(rawArgs); i++ {
		arg := rawArgs[i]
		if arg == "-r" || arg == "--random" {
			randomize = true
		} else if arg == "-l" || arg == "--limit" {
			if i+1 < len(rawArgs) {
				if val, err := strconv.Atoi(rawArgs[i+1]); err == nil {
					limit = val
				}
				i++
			}
		} else if arg == "--start-year" {
			if i+1 < len(rawArgs) {
				if val, err := strconv.Atoi(rawArgs[i+1]); err == nil {
					startYear = val
				}
				i++
			}
		} else if arg == "--end-year" {
			if i+1 < len(rawArgs) {
				if val, err := strconv.Atoi(rawArgs[i+1]); err == nil {
					endYear = val
				}
				i++
			}
		} else {
			args = append(args, arg)
		}
	}

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
		queryMusicTUI(cliQuery, randomize, limit)
	case "list":
		listMusic(startYear, endYear)
	case "skip":
		sendCommand("skip")
	case "stop":
		sendCommand("stop")
	default:
		log.Fatalf("Unknown mode: %s", args[0])
	}
}

/* ---------- TUI ---------- */
func queryMusicTUI(cliQuery string, randomize bool, limit int) {
	collection := connectToMongo()

	if err := ioutil.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
		log.Printf("Warning: Could not write PID file: %v", err)
	}
	defer os.Remove(pidFile)
	defer os.Remove(commandFile)

	err := termbox.Init()
	if err != nil {
		panic(err)
	}
	defer termbox.Close()

	state := &TUIState{
		query:        cliQuery,
		activeWindow: "query",
		artists:      []CountItem{},
		genres:       []CountItem{},
		years:        []CountItem{},
	}

	// If we have an initial query, run it
	if cliQuery != "" {
		runQuery(collection, state, randomize, limit)
	}

	eventChan := make(chan termbox.Event)
	go func() {
		for {
			eventChan <- termbox.PollEvent()
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGUSR1)

	commandChan := make(chan string, 1)
	go func() {
		for range sigChan {
			if b, err := ioutil.ReadFile(commandFile); err == nil {
				commandChan <- string(b)
				_ = os.Remove(commandFile)
			}
		}
	}()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var currentCmd *exec.Cmd
	var songs []Song
	var songIndex int
	var playbackActive bool
	playbackMutex := &sync.Mutex{}

	// Cleanup function to kill any running music process
	defer func() {
		if currentCmd != nil {
			currentCmd.Process.Kill()
		}
	}()

	for {
		select {
		case ev := <-eventChan:
			if ev.Type == termbox.EventKey {
				if handleKeyEvent(&ev, state, collection, randomize, limit, &currentCmd, &songs, &songIndex, &playbackActive, playbackMutex) {
					return
				}
			} else if ev.Type == termbox.EventResize {
				// Just redraw on resize
			}
		case cmd := <-commandChan:
			handleExternalCommand(cmd, &currentCmd, &songs, &songIndex, state, playbackMutex)
		case <-ticker.C:
			// Periodic redraw
		}
		drawUI(state)
	}
}

func handleKeyEvent(ev *termbox.Event, state *TUIState, collection *mongo.Collection, randomize bool, limit int, currentCmd **exec.Cmd, songs *[]Song, songIndex *int, playbackActive *bool, mu *sync.Mutex) bool {
	if state.showHelp {
		state.showHelp = false
		return false
	}

	// Handle query window mode separately
	if state.activeWindow == "query" {
		switch ev.Key {
		case termbox.KeyEsc, termbox.KeyCtrlC:
			state.activeWindow = "artist" // Exit query mode
			return false
		case termbox.KeyEnter:
			runQuery(collection, state, randomize, limit)
			state.activeWindow = "artist" // Exit query mode after running query
			// Start playback
			mu.Lock()
			if !*playbackActive {
				*playbackActive = true
				go playback(state, currentCmd, songs, songIndex, playbackActive, mu)
			}
			mu.Unlock()
		case termbox.KeySpace:
			// Space types a space in query mode
			state.query = state.query[:state.cursorPos] + " " + state.query[state.cursorPos:]
			state.cursorPos++
		case termbox.KeyBackspace, termbox.KeyBackspace2:
			if state.cursorPos > 0 {
				state.query = state.query[:state.cursorPos-1] + state.query[state.cursorPos:]
				state.cursorPos--
			}
		case termbox.KeyArrowLeft:
			if state.cursorPos > 0 {
				state.cursorPos--
			}
		case termbox.KeyArrowRight:
			if state.cursorPos < len(state.query) {
				state.cursorPos++
			}
		case termbox.KeyTab:
			state.activeWindow = "artist" // Allow tab to exit query mode
		default:
			if ev.Ch != 0 {
				state.query = state.query[:state.cursorPos] + string(ev.Ch) + state.query[state.cursorPos:]
				state.cursorPos++
			}
		}
		return false
	}

	// Handle all other windows
	switch ev.Key {
	case termbox.KeyEsc, termbox.KeyCtrlC:
		if *currentCmd != nil {
			(*currentCmd).Process.Kill()
		}
		return true
	case termbox.KeySpace:
		state.activeWindow = "query"
		state.cursorPos = len(state.query)
	case termbox.KeyArrowUp:
		scrollWindow(state, -1)
	case termbox.KeyArrowDown:
		scrollWindow(state, 1)
	default:
		if ev.Ch != 0 {
			switch ev.Ch {
			case 'q':
				if *currentCmd != nil {
					(*currentCmd).Process.Kill()
				}
				return true
			case 's':
				skipSong(currentCmd, songs, songIndex, state, mu)
			case 'a':
				state.activeWindow = "artist"
			case 'g':
				state.activeWindow = "genre"
			case 'y':
				state.activeWindow = "year"
			case 'h', '?':
				state.showHelp = true
			}
		}
	}
	return false
}

func scrollWindow(state *TUIState, delta int) {
	switch state.activeWindow {
	case "artist":
		state.artistScroll += delta
		if state.artistScroll < 0 {
			state.artistScroll = 0
		}
		if state.artistScroll >= len(state.artists) {
			state.artistScroll = len(state.artists) - 1
		}
	case "genre":
		state.genreScroll += delta
		if state.genreScroll < 0 {
			state.genreScroll = 0
		}
		if state.genreScroll >= len(state.genres) {
			state.genreScroll = len(state.genres) - 1
		}
	case "year":
		state.yearScroll += delta
		if state.yearScroll < 0 {
			state.yearScroll = 0
		}
		if state.yearScroll >= len(state.years) {
			state.yearScroll = len(state.years) - 1
		}
	}
}

func runQuery(collection *mongo.Collection, state *TUIState, randomize bool, limit int) {
	criteria := parseQuery(state.query)
	songs, err := searchSongs(collection, criteria)
	if err != nil {
		state.message = fmt.Sprintf("Error: %v", err)
		return
	}
	if len(songs) == 0 {
		state.message = "No songs found"
		return
	}

	if randomize {
		rand.Shuffle(len(songs), func(i, j int) { songs[i], songs[j] = songs[j], songs[i] })
	}
	if limit > 0 && len(songs) > limit {
		songs = songs[:limit]
	}

	// Update statistics
	artistCounts := make(map[string]int)
	genreCounts := make(map[string]int)
	yearCounts := make(map[int]int)

	for _, s := range songs {
		if s.Artist != "" {
			artistCounts[s.Artist]++
		}
		if s.Genre != "" {
			genreCounts[s.Genre]++
		}
		if s.Year > 0 {
			yearCounts[s.Year]++
		}
	}

	state.artists = mapToSortedSlice(artistCounts, true)
	state.genres = mapToSortedSlice(genreCounts, true)
	state.years = yearMapToSortedSlice(yearCounts)
	state.totalSongs = len(songs)
	state.playlist = songs // Store full playlist
	state.message = fmt.Sprintf("Found %d songs", len(songs))
}

func mapToSortedSlice(m map[string]int, descending bool) []CountItem {
	var items []CountItem
	for k, v := range m {
		items = append(items, CountItem{k, v})
	}
	sort.Slice(items, func(i, j int) bool {
		if descending {
			return items[i].Value > items[j].Value
		}
		return items[i].Value < items[j].Value
	})
	return items
}

func yearMapToSortedSlice(m map[int]int) []CountItem {
	var items []CountItem
	for k, v := range m {
		items = append(items, CountItem{strconv.Itoa(k), v})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Key < items[j].Key
	})
	return items
}

func drawUI(state *TUIState) {
	termbox.Clear(termbox.ColorDefault, termbox.ColorDefault)
	w, h := termbox.Size()

	if state.showHelp {
		drawHelp(w, h)
		termbox.Flush()
		return
	}

	// Top: Query line
	drawText(0, 0, "Query: ", termbox.ColorYellow|termbox.AttrBold, termbox.ColorDefault)
	queryX := 7
	drawText(queryX, 0, state.query, termbox.ColorWhite, termbox.ColorDefault)
	if state.activeWindow == "query" {
		termbox.SetCursor(queryX+state.cursorPos, 0)
	} else {
		termbox.HideCursor()
	}

	// Draw separator
	for x := 0; x < w; x++ {
		termbox.SetCell(x, 1, '─', termbox.ColorWhite, termbox.ColorDefault)
	}

	// Calculate section heights
	songInfoHeight := 6 // Compact song info at bottom
	// Queue section gets remaining space after middle and song info
	middleHeight := (h - 2 - songInfoHeight - 4) / 2 // -2 for top, -4 for separators
	if middleHeight < 5 {
		middleHeight = 5
	}
	queueHeight := h - 2 - middleHeight - songInfoHeight - 3 // What's left for queue
	if queueHeight < 3 {
		queueHeight = 3
	}

	middleY := 2
	queueY := middleY + middleHeight + 1
	bottomY := queueY + queueHeight + 1

	// Middle: Three columns
	colWidth := w / 3
	drawColumn(0, middleY, colWidth, middleHeight, "Artists (a)", state.artists, state.artistScroll, state.activeWindow == "artist")
	drawColumn(colWidth, middleY, colWidth, middleHeight, "Genres (g)", state.genres, state.genreScroll, state.activeWindow == "genre")
	drawColumn(colWidth*2, middleY, w-colWidth*2, middleHeight, "Years (y)", state.years, state.yearScroll, state.activeWindow == "year")

	// Draw separator before queue section
	for x := 0; x < w; x++ {
		termbox.SetCell(x, queueY-1, '─', termbox.ColorWhite, termbox.ColorDefault)
	}

	// Queue: Upcoming songs
	drawQueue(0, queueY, w, queueHeight, state)

	// Draw separator before bottom
	for x := 0; x < w; x++ {
		termbox.SetCell(x, bottomY-1, '─', termbox.ColorWhite, termbox.ColorDefault)
	}

	// Bottom: Current song info
	drawSongInfo(0, bottomY, w, state)

	// Status line at very bottom
	statusY := h - 1
	status := fmt.Sprintf(" [Space]=Query [Enter]=Run [Esc]=Exit Query [a/g/y]=Windows [↑↓]=Scroll [s]=Skip [q]=Quit [?]=Help | %s", state.message)
	drawText(0, statusY, status, termbox.ColorBlack, termbox.ColorWhite)

	termbox.Flush()
}

func drawHelp(w, h int) {
	help := []string{
		"DeeJay TUI - Keyboard Commands",
		"",
		"  Space   - Jump to query window",
		"  Enter   - Execute query and play",
		"  Esc     - Exit query window",
		"  q       - Quit (when not in query window)",
		"  s       - Skip current song",
		"  a       - Switch to Artists window",
		"  g       - Switch to Genres window",
		"  y       - Switch to Years window",
		"  ↑/↓     - Scroll in active window",
		"  h or ?  - Show this help",
		"  Esc     - Close help / Quit",
		"",
		"Press any key to close help...",
	}

	startY := (h - len(help)) / 2
	for i, line := range help {
		x := (w - len(line)) / 2
		if x < 0 {
			x = 0
		}
		drawText(x, startY+i, line, termbox.ColorWhite, termbox.ColorDefault)
	}
}

func drawColumn(x, y, w, h int, title string, items []CountItem, scroll int, active bool) {
	fg := termbox.ColorWhite
	if active {
		fg = termbox.ColorYellow | termbox.AttrBold
	}
	drawText(x, y, title, fg, termbox.ColorDefault)

	// Draw vertical separator
	if x > 0 {
		for dy := 0; dy < h; dy++ {
			termbox.SetCell(x-1, y+dy, '│', termbox.ColorWhite, termbox.ColorDefault)
		}
	}

	visibleHeight := h - 1
	for i := 0; i < visibleHeight && scroll+i < len(items); i++ {
		item := items[scroll+i]
		line := fmt.Sprintf("  %s: %d", item.Key, item.Value)
		if len(line) > w-1 {
			line = line[:w-4] + "..."
		}
		drawText(x, y+1+i, line, termbox.ColorWhite, termbox.ColorDefault)
	}

	// Show scroll indicators
	if scroll > 0 {
		drawText(x+w-3, y, "▲", termbox.ColorCyan, termbox.ColorDefault)
	}
	if scroll+visibleHeight < len(items) {
		drawText(x+w-3, y+h-1, "▼", termbox.ColorCyan, termbox.ColorDefault)
	}
}

func drawQueue(x, y, w, h int, state *TUIState) {
	title := "Up Next"
	drawText(x, y, title, termbox.ColorCyan|termbox.AttrBold, termbox.ColorDefault)

	if len(state.playlist) == 0 || state.songIndex >= len(state.playlist)-1 {
		drawText(x+2, y+1, "No upcoming songs", termbox.ColorWhite|termbox.AttrDim, termbox.ColorDefault)
		return
	}

	// Show upcoming songs starting from next song
	visibleHeight := h - 1
	nextIdx := state.songIndex + 1

	for i := 0; i < visibleHeight && nextIdx+i < len(state.playlist); i++ {
		song := state.playlist[nextIdx+i]
		line := fmt.Sprintf("  %d. %s - %s", nextIdx+i+1, song.Artist, song.Title)
		if song.Album != "" {
			line += fmt.Sprintf(" (%s)", song.Album)
		}
		if len(line) > w-1 {
			line = line[:w-4] + "..."
		}
		drawText(x, y+1+i, line, termbox.ColorWhite, termbox.ColorDefault)
	}

	// Show indicator if more songs exist
	if nextIdx+visibleHeight < len(state.playlist) {
		remaining := len(state.playlist) - (nextIdx + visibleHeight)
		indicator := fmt.Sprintf("  ... and %d more", remaining)
		drawText(x, y+h-1, indicator, termbox.ColorCyan|termbox.AttrDim, termbox.ColorDefault)
	}
}

func drawSongInfo(x, y, w int, state *TUIState) {
	if state.currentSong == nil {
		drawText(x, y, "No song playing", termbox.ColorWhite|termbox.AttrDim, termbox.ColorDefault)
		return
	}

	s := state.currentSong

	// Compact format to save space
	line1 := fmt.Sprintf("♪ [%d/%d] %s", state.songIndex+1, state.totalSongs, s.Title)
	line2 := fmt.Sprintf("  %s", s.Artist)
	if s.Album != "" {
		line2 += fmt.Sprintf(" • %s", s.Album)
	}
	if s.Year > 0 {
		line2 += fmt.Sprintf(" • %d", s.Year)
	}
	if s.Genre != "" {
		line2 += fmt.Sprintf(" • %s", s.Genre)
	}

	if len(line1) > w-1 {
		line1 = line1[:w-4] + "..."
	}
	if len(line2) > w-1 {
		line2 = line2[:w-4] + "..."
	}

	drawText(x, y, line1, termbox.ColorGreen|termbox.AttrBold, termbox.ColorDefault)
	drawText(x, y+1, line2, termbox.ColorWhite, termbox.ColorDefault)
}

func drawText(x, y int, text string, fg, bg termbox.Attribute) {
	for i, ch := range text {
		termbox.SetCell(x+i, y, ch, fg, bg)
	}
}

func playback(state *TUIState, currentCmd **exec.Cmd, songs *[]Song, songIndex *int, playbackActive *bool, mu *sync.Mutex) {
	defer func() {
		mu.Lock()
		*playbackActive = false
		mu.Unlock()
	}()

	mu.Lock()
	*songs = state.playlist
	*songIndex = 0
	mu.Unlock()

	for {
		mu.Lock()
		if *songIndex >= len(*songs) {
			mu.Unlock()
			break
		}
		song := (*songs)[*songIndex]
		state.currentSong = &song
		state.songIndex = *songIndex
		mu.Unlock()

		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.Command("afplay", song.ID)
		case "linux":
			if _, err := exec.LookPath("mpg123"); err == nil {
				cmd = exec.Command("mpg123", "-q", song.ID)
			} else if _, err := exec.LookPath("ffplay"); err == nil {
				cmd = exec.Command("ffplay", "-nodisp", "-autoexit", "-loglevel", "quiet", song.ID)
			}
		case "windows":
			cmd = exec.Command("cmd", "/C", "start", "/wait", song.ID)
		}
		if cmd == nil {
			state.message = "No media player found"
			return
		}

		mu.Lock()
		*currentCmd = cmd
		mu.Unlock()

		_ = cmd.Start()
		_ = cmd.Wait()

		mu.Lock()
		*currentCmd = nil
		*songIndex++
		mu.Unlock()
	}

	state.currentSong = nil
	state.message = "Playback finished"
}

func skipSong(currentCmd **exec.Cmd, songs *[]Song, songIndex *int, state *TUIState, mu *sync.Mutex) {
	mu.Lock()
	defer mu.Unlock()

	if *currentCmd != nil {
		// Just kill the current song - playback loop will advance naturally
		(*currentCmd).Process.Kill()
		state.message = "Skipped"
	}
}

func handleExternalCommand(cmd string, currentCmd **exec.Cmd, songs *[]Song, songIndex *int, state *TUIState, mu *sync.Mutex) {
	switch cmd {
	case "skip":
		skipSong(currentCmd, songs, songIndex, state, mu)
	case "stop":
		if *currentCmd != nil {
			(*currentCmd).Process.Kill()
			*currentCmd = nil
		}
		state.currentSong = nil
		state.message = "Stopped by external command"
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

/* ---------- list ---------- */
func listMusic(startYear, endYear int) {
	collection := connectToMongo()

	filter := bson.M{}
	if startYear > 0 && endYear > 0 {
		filter["year"] = bson.M{"$gte": startYear, "$lte": endYear}
	} else if startYear > 0 {
		filter["year"] = bson.M{"$gte": startYear}
	} else if endYear > 0 {
		filter["year"] = bson.M{"$lte": endYear}
	}

	cur, err := collection.Find(context.Background(), filter)
	if err != nil {
		log.Fatalf("Error listing songs: %v", err)
	}
	defer cur.Close(context.Background())

	var songs []Song
	if err := cur.All(context.Background(), &songs); err != nil {
		log.Fatalf("Error decoding songs: %v", err)
	}

	if len(songs) == 0 {
		fmt.Println("No songs found in database.")
		return
	}

	artistCounts := make(map[string]int)
	genreCounts := make(map[string]int)
	yearCounts := make(map[int]int)

	for _, s := range songs {
		if s.Artist != "" {
			artistCounts[s.Artist]++
		}
		if s.Genre != "" {
			genreCounts[s.Genre]++
		}
		if s.Year > 0 {
			yearCounts[s.Year]++
		}
	}

	fmt.Printf("\n=== Music Library Summary ===\n")
	if startYear > 0 || endYear > 0 {
		if startYear > 0 && endYear > 0 {
			fmt.Printf("Years: %d - %d\n", startYear, endYear)
		} else if startYear > 0 {
			fmt.Printf("Years: %d and later\n", startYear)
		} else {
			fmt.Printf("Years: up to %d\n", endYear)
		}
	}
	fmt.Printf("Total Songs: %d\n", len(songs))
	fmt.Printf("Total Artists: %d\n", len(artistCounts))
	fmt.Printf("Total Genres: %d\n\n", len(genreCounts))

	fmt.Println("--- Top 20 Artists ---")
	artistList := mapToSortedSlice(artistCounts, true)
	limit := 20
	if len(artistList) < limit {
		limit = len(artistList)
	}
	for i := 0; i < limit; i++ {
		fmt.Printf("  %s: %d\n", artistList[i].Key, artistList[i].Value)
	}

	fmt.Println("\n--- Top 20 Genres ---")
	genreList := mapToSortedSlice(genreCounts, true)
	limit = 20
	if len(genreList) < limit {
		limit = len(genreList)
	}
	for i := 0; i < limit; i++ {
		fmt.Printf("  %s: %d\n", genreList[i].Key, genreList[i].Value)
	}

	fmt.Println("\n--- Songs by Year ---")
	yearList := yearMapToSortedSlice(yearCounts)
	for _, item := range yearList {
		fmt.Printf("  %s: %d\n", item.Key, item.Value)
	}
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
		if m := regexp.MustCompile(`(?:in|from|year)\s+(\d{4})`).FindStringSubmatch(q); len(m) > 1 {
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
