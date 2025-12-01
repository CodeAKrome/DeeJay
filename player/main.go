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
	message      string
	showHelp     bool
	confirmQuit  bool   // for confirm-on-quit
	playlist     []Song // Store current playlist
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

/* ---------- TUI + control protocol ---------- */

type playerStatus struct {
	Song  *Song
	Index int
	Total int
	Msg   string
}

type controlMsg struct {
	Cmd   string
	Songs []Song
	Reply chan playerStatus
}

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

	termbox.SetInputMode(termbox.InputEsc | termbox.InputMouse)

	state := &TUIState{
		query:        cliQuery,
		activeWindow: "query",
		artists:      []CountItem{},
		genres:       []CountItem{},
		years:        []CountItem{},
		playlist:     []Song{}, // Initialize empty playlist
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controlChan := make(chan controlMsg, 8)
	go playbackManager(ctx, controlChan)

	if cliQuery != "" {
		if songs, err := runQueryAndReturn(collection, state, randomize, limit); err == nil && len(songs) > 0 {
			controlChan <- controlMsg{Cmd: "play", Songs: songs}
		}
	}

	eventCtx, eventCancel := context.WithCancel(context.Background())
	defer eventCancel()

	eventChan := make(chan termbox.Event, 100)
	go func() {
		for {
			select {
			case <-eventCtx.Done():
				return
			default:
				ev := termbox.PollEvent()
				select {
				case eventChan <- ev:
				case <-eventCtx.Done():
					return
				default:
					log.Printf("Event channel full, dropping event")
				}
			}
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGUSR1)

	commandFileChan := make(chan string, 1)
	go func() {
		for {
			select {
			case <-sigChan:
				if b, err := ioutil.ReadFile(commandFile); err == nil {
					commandFileChan <- string(b)
					_ = os.Remove(commandFile)
				}
			case <-eventCtx.Done():
				return
			}
		}
	}()

	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	drawUI(state)

	for {
		select {
		case ev := <-eventChan:
			if ev.Type == termbox.EventKey {
				quit := handleKeyEvent(&ev, state, collection, randomize, limit, controlChan)
				if quit {
					eventCancel()
					controlChan <- controlMsg{Cmd: "quit"}
					cancel()
					time.Sleep(100 * time.Millisecond)
					return
				}
				drawUI(state)
			} else if ev.Type == termbox.EventResize {
				drawUI(state)
			} else if ev.Type == termbox.EventError {
				log.Printf("Termbox error: %v", ev.Err)
			}
		case cmd := <-commandFileChan:
			switch strings.TrimSpace(cmd) {
			case "skip":
				controlChan <- controlMsg{Cmd: "skip"}
			case "stop":
				controlChan <- controlMsg{Cmd: "stop"}
			}
		case <-ticker.C:
			reply := make(chan playerStatus, 1)
			controlChan <- controlMsg{Cmd: "status", Reply: reply}
			select {
			case st := <-reply:
				state.currentSong = st.Song
				state.songIndex = st.Index
				state.totalSongs = st.Total
				if st.Msg != "" {
					state.message = st.Msg
				}
				drawUI(state)
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

func handleKeyEvent(ev *termbox.Event, state *TUIState, collection *mongo.Collection, randomize bool, limit int, controlChan chan<- controlMsg) bool {
	resetConfirm := func() {
		if state.confirmQuit {
			state.confirmQuit = false
			state.message = ""
		}
	}

	if state.showHelp {
		state.showHelp = false
		return false
	}

	switch ev.Key {
	case termbox.KeyEsc, termbox.KeyCtrlC:
		return true
	case termbox.KeySpace:
		resetConfirm()
		state.activeWindow = "query"
		state.cursorPos = len(state.query)
	case termbox.KeyEnter:
		resetConfirm()
		if state.activeWindow == "query" {
			if songs, err := runQueryAndReturn(collection, state, randomize, limit); err == nil && len(songs) > 0 {
				controlChan <- controlMsg{Cmd: "play", Songs: songs}
				state.message = fmt.Sprintf("Playing %d songs", len(songs))
			}
		}
	case termbox.KeyBackspace, termbox.KeyBackspace2:
		resetConfirm()
		if state.activeWindow == "query" && state.cursorPos > 0 {
			state.query = state.query[:state.cursorPos-1] + state.query[state.cursorPos:]
			state.cursorPos--
		}
	case termbox.KeyArrowLeft:
		resetConfirm()
		if state.activeWindow == "query" && state.cursorPos > 0 {
			state.cursorPos--
		}
	case termbox.KeyArrowRight:
		resetConfirm()
		if state.activeWindow == "query" && state.cursorPos < len(state.query) {
			state.cursorPos++
		}
	case termbox.KeyArrowUp:
		resetConfirm()
		scrollWindow(state, -1)
	case termbox.KeyArrowDown:
		resetConfirm()
		scrollWindow(state, 1)
	default:
		if ev.Ch != 0 {
			switch ev.Ch {
			case 'q':
				if state.activeWindow != "query" {
					if !state.confirmQuit {
						state.confirmQuit = true
						state.message = "Press q again to quit"
						return false
					}
					return true
				}
				resetConfirm()
				return false
			case 's':
				resetConfirm()
				controlChan <- controlMsg{Cmd: "skip"}
				return false
			case 'a':
				resetConfirm()
				state.activeWindow = "artist"
				return false
			case 'g':
				resetConfirm()
				state.activeWindow = "genre"
				return false
			case 'y':
				resetConfirm()
				state.activeWindow = "year"
				return false
			case 'h', '?':
				resetConfirm()
				state.showHelp = true
				return false
			default:
				resetConfirm()
				if state.activeWindow == "query" {
					state.query = state.query[:state.cursorPos] + string(ev.Ch) + state.query[state.cursorPos:]
					state.cursorPos++
				}
			}
		}
	}
	return false
}

func scrollWindow(state *TUIState, delta int) {
	switch state.activeWindow {
	case "artist":
		if len(state.artists) == 0 {
			return
		}
		state.artistScroll += delta
		if state.artistScroll < 0 {
			state.artistScroll = 0
		}
		if state.artistScroll >= len(state.artists) {
			state.artistScroll = len(state.artists) - 1
		}
	case "genre":
		if len(state.genres) == 0 {
			return
		}
		state.genreScroll += delta
		if state.genreScroll < 0 {
			state.genreScroll = 0
		}
		if state.genreScroll >= len(state.genres) {
			state.genreScroll = len(state.genres) - 1
		}
	case "year":
		if len(state.years) == 0 {
			return
		}
		state.yearScroll += delta
		if state.yearScroll < 0 {
			state.yearScroll = 0
		}
		if state.yearScroll >= len(state.years) {
			state.yearScroll = len(state.years) - 1
		}
	}
}

func runQueryAndReturn(collection *mongo.Collection, state *TUIState, randomize bool, limit int) ([]Song, error) {
	criteria := parseQuery(state.query)
	songs, err := searchSongs(collection, criteria)
	if err != nil {
		state.message = fmt.Sprintf("Error: %v", err)
		return nil, err
	}
	if len(songs) == 0 {
		state.message = "No songs found"
		state.artists = nil
		state.genres = nil
		state.years = nil
		state.totalSongs = 0
		state.playlist = nil // Clear playlist
		return nil, nil
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
	state.message = fmt.Sprintf("Found %d songs", len(songs))
	state.playlist = songs // Store playlist

	return songs, nil
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

	// Calculate middle section height (leave space for bottom section + status)
	bottomHeight := 10                                           // Lines for Playing Next (title + 9 songs)
	statusPanelHeight := 4                                       // Lines for Now Playing details
	middleHeight := h - 2 - bottomHeight - statusPanelHeight - 2 // -2 separators
	if middleHeight < 5 {
		middleHeight = 5
	}

	middleY := 2
	bottomY := middleY + middleHeight + 1
	statusY := bottomY + bottomHeight

	// Middle: Three columns
	colWidth := w / 3
	drawColumn(0, middleY, colWidth, middleHeight, "Artists (a)", state.artists, state.artistScroll, state.activeWindow == "artist")
	drawColumn(colWidth, middleY, colWidth, middleHeight, "Genres (g)", state.genres, state.genreScroll, state.activeWindow == "genre")
	drawColumn(colWidth*2, middleY, w-colWidth*2, middleHeight, "Years (y)", state.years, state.yearScroll, state.activeWindow == "year")

	// Draw separator before bottom
	for x := 0; x < w; x++ {
		termbox.SetCell(x, bottomY-1, '─', termbox.ColorWhite, termbox.ColorDefault)
	}

	// Bottom: Playing Next (upcoming songs)
	drawSongInfo(0, bottomY, w, state)

	// Draw separator before status panel
	for x := 0; x < w; x++ {
		termbox.SetCell(x, statusY-1, '─', termbox.ColorWhite, termbox.ColorDefault)
	}

	// Status panel: Current song details
	drawStatusPanel(0, statusY, w, state)

	// Status line at very bottom
	footerY := h - 1
	footer := fmt.Sprintf(" [Space]=Query [a/g/y]=Windows [↑↓]=Scroll [s]=Skip [q]=Quit [?]=Help | %s", state.message)
	if len(footer) > w {
		footer = footer[:w]
	}
	drawText(0, footerY, footer, termbox.ColorBlack, termbox.ColorWhite)

	termbox.Flush()
}

func drawHelp(w, h int) {
	help := []string{
		"DeeJay TUI - Keyboard Commands",
		"",
		"  Space   - Jump to query window",
		"  Enter   - Execute query and play",
		"  q       - Quit (press twice to confirm when not in query)",
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
	bg := termbox.ColorDefault
	if active {
		bg = termbox.ColorDarkGray
	}

	for dx := 0; dx < w; dx++ {
		for dy := 0; dy < h; dy++ {
			termbox.SetCell(x+dx, y+dy, ' ', termbox.ColorWhite, bg)
		}
	}

	fg := termbox.ColorWhite
	if active {
		fg = termbox.ColorYellow | termbox.AttrBold
	}
	drawText(x, y, title, fg, bg)

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
		drawText(x, y+1+i, line, termbox.ColorWhite, bg)
	}

	if scroll > 0 {
		drawText(x+w-3, y, "▲", termbox.ColorCyan, bg)
	}
	if scroll+visibleHeight < len(items) {
		drawText(x+w-3, y+h-1, "▼", termbox.ColorCyan, bg)
	}
}

func drawSongInfo(x, y, w int, state *TUIState) {
	// Clear the Playing Next area (10 lines)
	for dy := 0; dy < 10; dy++ {
		for dx := 0; dx < w; dx++ {
			termbox.SetCell(x+dx, y+dy, ' ', termbox.ColorWhite, termbox.ColorDefault)
		}
	}

	title := "Playing Next"
	drawText(x, y, title, termbox.ColorYellow|termbox.AttrBold, termbox.ColorDefault)

	if state.playlist == nil || len(state.playlist) == 0 {
		drawText(x, y+1, "No playlist loaded", termbox.ColorWhite, termbox.ColorDefault)
		return
	}

	// Show next 9 songs (title takes 1 line, total 10 lines)
	startIdx := state.songIndex + 1
	if startIdx >= len(state.playlist) {
		drawText(x, y+1, "End of playlist", termbox.ColorWhite, termbox.ColorDefault)
		return
	}

	endIdx := startIdx + 9
	if endIdx > len(state.playlist) {
		endIdx = len(state.playlist)
	}

	for i := startIdx; i < endIdx; i++ {
		song := state.playlist[i]
		// Format: "Artist - Title"
		line := fmt.Sprintf(" %d. %s - %s", i+1, song.Artist, song.Title)
		if len(line) > w-1 {
			line = line[:w-4] + "..."
		}
		drawText(x, y+1+(i-startIdx), line, termbox.ColorWhite, termbox.ColorDefault)
	}
}

func drawStatusPanel(x, y, w int, state *TUIState) {
	// Clear the status panel area (4 lines)
	for dy := 0; dy < 4; dy++ {
		for dx := 0; dx < w; dx++ {
			termbox.SetCell(x+dx, y+dy, ' ', termbox.ColorWhite, termbox.ColorDefault)
		}
	}

	// Title
	drawText(x+1, y, " Status ", termbox.ColorCyan|termbox.AttrBold, termbox.ColorDefault)

	if state.currentSong == nil {
		drawText(x+1, y+1, "Idle", termbox.ColorWhite, termbox.ColorDefault)
		// Show message if any
		if state.message != "" {
			msg := state.message
			if len(msg) > w-4 {
				msg = msg[:w-7] + "..."
			}
			drawText(x+1, y+2, msg, termbox.ColorBlack, termbox.ColorWhite)
		}
		return
	}

	s := state.currentSong

	// Line 1: [index/total] Now Playing:
	line1 := fmt.Sprintf("[%d/%d] Now Playing:", state.songIndex+1, state.totalSongs)
	drawText(x+1, y+1, line1, termbox.ColorWhite, termbox.ColorDefault)

	// Line 2: Title
	line2 := fmt.Sprintf(" Title: %s", s.Title)
	if len(line2) > w-4 {
		line2 = line2[:w-7] + "..."
	}
	drawText(x+1, y+2, line2, termbox.ColorWhite, termbox.ColorDefault)

	// Line 3: Artist - Album
	line3 := fmt.Sprintf(" Artist: %s  |  Album: %s", s.Artist, s.Album)
	if len(line3) > w-4 {
		line3 = line3[:w-7] + "..."
	}
	drawText(x+1, y+3, line3, termbox.ColorWhite, termbox.ColorDefault)

	// Optional 4th line with Year, Genre, Track (if space permits)
	extra := ""
	if s.Year > 0 {
		extra += fmt.Sprintf("Year: %d  ", s.Year)
	}
	if s.Genre != "" {
		extra += fmt.Sprintf("Genre: %s  ", s.Genre)
	}
	if s.TrackNum > 0 {
		if s.TrackTotal > 0 {
			extra += fmt.Sprintf("Track: %d/%d", s.TrackNum, s.TrackTotal)
		} else {
			extra += fmt.Sprintf("Track: %d", s.TrackNum)
		}
	}
	if extra != "" && w > 60 { // Only show if terminal is wide enough
		line4 := " " + extra
		if len(line4) > w-4 {
			line4 = line4[:w-7] + "..."
		}
		drawText(x+1, y+4, line4, termbox.ColorWhite, termbox.ColorDefault)
	}
}

func drawText(x, y int, text string, fg, bg termbox.Attribute) {
	for i, ch := range text {
		termbox.SetCell(x+i, y, ch, fg, bg)
	}
}

/* ---------- Playback manager (rewritten, owns subprocesses) ---------- */

func playbackManager(ctx context.Context, control <-chan controlMsg) {
	var playlist []Song
	index := 0
	var currentCmd *exec.Cmd
	var mu sync.Mutex

	statusMsg := ""
	for {
		if playlist == nil || index >= len(playlist) {
			statusMsg = "Idle"
			select {
			case <-ctx.Done():
				mu.Lock()
				if currentCmd != nil && currentCmd.Process != nil {
					_ = currentCmd.Process.Kill()
				}
				mu.Unlock()
				return
			case msg := <-control:
				switch msg.Cmd {
				case "play":
					if len(msg.Songs) == 0 {
						continue
					}
					playlist = make([]Song, len(msg.Songs))
					copy(playlist, msg.Songs)
					index = 0
					statusMsg = fmt.Sprintf("Queued %d songs", len(playlist))
				case "status":
					if msg.Reply != nil {
						msg.Reply <- playerStatus{
							Song:  nil,
							Index: 0,
							Total: 0,
							Msg:   statusMsg,
						}
					}
				case "quit":
					mu.Lock()
					if currentCmd != nil && currentCmd.Process != nil {
						_ = currentCmd.Process.Kill()
					}
					mu.Unlock()
					return
				case "stop":
					playlist = nil
					index = 0
					statusMsg = "Stopped"
				default:
				}
			}
			continue
		}

		song := playlist[index]
		statusMsg = fmt.Sprintf("Playing %d/%d: %s - %s", index+1, len(playlist), song.Artist, song.Title)

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
		default:
			statusMsg = "No media player found for this OS"
			playlist = nil
			index = 0
			continue
		}

		if cmd == nil {
			statusMsg = "No media player found"
			playlist = nil
			index = 0
			continue
		}

		if err := cmd.Start(); err != nil {
			statusMsg = fmt.Sprintf("Failed to start player: %v", err)
			index++
			continue
		}

		mu.Lock()
		currentCmd = cmd
		mu.Unlock()

		done := make(chan error, 1)
		go func(c *exec.Cmd) { done <- c.Wait() }(cmd)

	waitLoop:
		for {
			select {
			case <-ctx.Done():
				mu.Lock()
				if currentCmd != nil && currentCmd.Process != nil {
					_ = currentCmd.Process.Kill()
				}
				mu.Unlock()
				<-done
				return
			case msg := <-control:
				switch msg.Cmd {
				case "skip":
					mu.Lock()
					if currentCmd != nil && currentCmd.Process != nil {
						_ = currentCmd.Process.Kill()
					}
					mu.Unlock()
					<-done
					index++
					break waitLoop
				case "stop":
					mu.Lock()
					if currentCmd != nil && currentCmd.Process != nil {
						_ = currentCmd.Process.Kill()
					}
					mu.Unlock()
					<-done
					playlist = nil
					index = 0
					statusMsg = "Stopped"
					break waitLoop
				case "play":
					mu.Lock()
					if currentCmd != nil && currentCmd.Process != nil {
						_ = currentCmd.Process.Kill()
					}
					mu.Unlock()
					<-done
					playlist = make([]Song, len(msg.Songs))
					copy(playlist, msg.Songs)
					index = 0
					statusMsg = fmt.Sprintf("Queued %d songs", len(playlist))
					break waitLoop
				case "status":
					if msg.Reply != nil {
						var curSong *Song
						cur := song
						curSong = &cur
						msg.Reply <- playerStatus{
							Song:  curSong,
							Index: index,
							Total: len(playlist),
							Msg:   statusMsg,
						}
					}
				case "quit":
					mu.Lock()
					if currentCmd != nil && currentCmd.Process != nil {
						_ = currentCmd.Process.Kill()
					}
					mu.Unlock()
					<-done
					return
				default:
				}
			case err := <-done:
				if err != nil {
					statusMsg = fmt.Sprintf("Player exited: %v", err)
				}
				index++
				break waitLoop
			}
		}

		mu.Lock()
		currentCmd = nil
		mu.Unlock()
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

/* ---------- control commands (external) ---------- */

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
