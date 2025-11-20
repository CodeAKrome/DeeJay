// Add these new fields to TUIState struct:
type TUIState struct {
	query        string
	cursorPos    int
	activeWindow string // "query", "artist", "genre", "year", "filter"
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

	// NEW: Filter state
	filterMode       bool   // Whether we're in filter mode
	filterType       string // "artist", "genre", "year"
	filterValue      string // The selected filter value
	filteredPlaylist []Song // Filtered subset of songs
	originalPlaylist []Song // Backup of original playlist
	originalIndex    int    // Where we were in original playlist
}

// Add this helper function to filter the playlist
func filterPlaylist(songs []Song, filterType, filterValue string) []Song {
	var filtered []Song
	filterLower := strings.ToLower(filterValue)

	for _, song := range songs {
		match := false
		switch filterType {
		case "artist":
			match = strings.ToLower(song.Artist) == filterLower
		case "genre":
			match = strings.ToLower(song.Genre) == filterLower
		case "year":
			match = strconv.Itoa(song.Year) == filterValue
		}
		if match {
			filtered = append(filtered, song)
		}
	}
	return filtered
}

// Modified handleKeyEvent to support filtering
// func handleKeyEvent(ev *termbox.Event, state *TUIState, collection *mongo.Collection, randomize bool, limit int, currentCmd **exec.Cmd, songs *[]Song, songIndex *int, playbackActive *bool, mu *sync.Mutex) bool {
// 	if state.showHelp {
// 		state.showHelp = false
// 		return false
// 	}

// 	// Handle filter mode
// 	if state.filterMode {
// 		switch ev.Key {
// 		case termbox.KeyEsc:
// 			// Exit filter mode and restore original playlist
// 			if state.originalPlaylist != nil {
// 				mu.Lock()
// 				state.playlist = state.originalPlaylist
// 				*songs = state.originalPlaylist
// 				*songIndex = state.originalIndex
// 				state.totalSongs = len(state.originalPlaylist)

// 				// Update current song reference
// 				if *songIndex < len(*songs) {
// 					state.currentSong = &(*songs)[*songIndex]
// 					state.songIndex = *songIndex
// 				}
// 				mu.Unlock()

// 				state.originalPlaylist = nil
// 				state.filteredPlaylist = nil
// 			}
// 			state.filterMode = false
// 			state.activeWindow = state.filterType // Return to the window we filtered from
// 			state.message = "Filter cleared - returned to full playlist"
// 			return false

// 		case termbox.KeyEnter:
// 			// Apply the filter and start playback
// 			var filterValue string
// 			var scroll int

// 			switch state.filterType {
// 			case "artist":
// 				if state.artistScroll < len(state.artists) {
// 					filterValue = state.artists[state.artistScroll].Key
// 				}
// 				scroll = state.artistScroll
// 			case "genre":
// 				if state.genreScroll < len(state.genres) {
// 					filterValue = state.genres[state.genreScroll].Key
// 				}
// 				scroll = state.genreScroll
// 			case "year":
// 				if state.yearScroll < len(state.years) {
// 					filterValue = state.years[state.yearScroll].Key
// 				}
// 				scroll = state.yearScroll
// 			}

// 			if filterValue != "" {
// 				filtered := filterPlaylist(state.playlist, state.filterType, filterValue)

// 				if len(filtered) > 0 {
// 					mu.Lock()
// 					// Save original state
// 					state.originalPlaylist = state.playlist
// 					state.originalIndex = *songIndex

// 					// Apply filter
// 					state.filteredPlaylist = filtered
// 					state.playlist = filtered
// 					*songs = filtered
// 					*songIndex = 0
// 					state.songIndex = 0
// 					state.totalSongs = len(filtered)
// 					state.currentSong = &filtered[0]
// 					state.filterValue = filterValue

// 					// Kill current song if playing
// 					if *currentCmd != nil {
// 						(*currentCmd).Process.Kill()
// 					}

// 					// Start playback of filtered playlist
// 					if !*playbackActive {
// 						*playbackActive = true
// 						go playback(state, currentCmd, songs, songIndex, playbackActive, mu)
// 					}
// 					mu.Unlock()

// 					state.message = fmt.Sprintf("Playing %d songs: %s = %s (Esc to restore)", len(filtered), state.filterType, filterValue)
// 				} else {
// 					state.message = "No songs match this filter"
// 				}
// 			}
// 			state.filterMode = false
// 			state.activeWindow = state.filterType
// 			return false

// 		case termbox.KeyArrowUp:
// 			scrollWindow(state, -1)
// 			return false
// 		case termbox.KeyArrowDown:
// 			scrollWindow(state, 1)
// 			return false
// 		}
// 		return false
// 	}

// 	// Handle query window mode separately
// 	if state.activeWindow == "query" {
// 		switch ev.Key {
// 		case termbox.KeyEsc, termbox.KeyCtrlC:
// 			state.activeWindow = "artist"
// 			return false
// 		case termbox.KeyEnter:
// 			runQuery(collection, state, randomize, limit)
// 			state.activeWindow = "artist"
// 			mu.Lock()
// 			if !*playbackActive {
// 				*playbackActive = true
// 				go playback(state, currentCmd, songs, songIndex, playbackActive, mu)
// 			}
// 			mu.Unlock()
// 		case termbox.KeySpace:
// 			state.query = state.query[:state.cursorPos] + " " + state.query[state.cursorPos:]
// 			state.cursorPos++
// 		case termbox.KeyBackspace, termbox.KeyBackspace2:
// 			if state.cursorPos > 0 {
// 				state.query = state.query[:state.cursorPos-1] + state.query[state.cursorPos:]
// 				state.cursorPos--
// 			}
// 		case termbox.KeyArrowLeft:
// 			if state.cursorPos > 0 {
// 				state.cursorPos--
// 			}
// 		case termbox.KeyArrowRight:
// 			if state.cursorPos < len(state.query) {
// 				state.cursorPos++
// 			}
// 		case termbox.KeyTab:
// 			state.activeWindow = "artist"
// 		default:
// 			if ev.Ch != 0 {
// 				state.query = state.query[:state.cursorPos] + string(ev.Ch) + state.query[state.cursorPos:]
// 				state.cursorPos++
// 			}
// 		}
// 		return false
// 	}

// 	// Handle all other windows
// 	switch ev.Key {
// 	case termbox.KeyEsc, termbox.KeyCtrlC:
// 		if *currentCmd != nil {
// 			(*currentCmd).Process.Kill()
// 		}
// 		return true
// 	case termbox.KeySpace:
// 		state.activeWindow = "query"
// 		state.cursorPos = len(state.query)
// 	case termbox.KeyEnter:
// 		// NEW: Enter in a column window activates filter mode
// 		if len(state.playlist) > 0 {
// 			state.filterMode = true
// 			state.filterType = state.activeWindow
// 			state.message = fmt.Sprintf("Filter mode: Select %s and press Enter to play, Esc to cancel", state.activeWindow)
// 		}
// 	case termbox.KeyArrowUp:
// 		scrollWindow(state, -1)
// 	case termbox.KeyArrowDown:
// 		scrollWindow(state, 1)
// 	default:
// 		if ev.Ch != 0 {
// 			switch ev.Ch {
// 			case 'q':
// 				if *currentCmd != nil {
// 					(*currentCmd).Process.Kill()
// 				}
// 				return true
// 			case 's':
// 				skipSong(currentCmd, songs, songIndex, state, mu)
// 			case 'a':
// 				state.activeWindow = "artist"
// 			case 'g':
// 				state.activeWindow = "genre"
// 			case 'y':
// 				state.activeWindow = "year"
// 			case 'h', '?':
// 				state.showHelp = true
// 			}
// 		}
// 	}
// 	return false
// }

// Modified handleKeyEvent to support filtering and graceful exit
func handleKeyEvent(ev *termbox.Event, state *TUIState, collection *mongo.Collection, randomize bool, limit int, currentCmd **exec.Cmd, songs *[]Song, songIndex *int, playbackActive *bool, mu *sync.Mutex) bool {
	if state.showHelp {
		state.showHelp = false
		return false
	}

	// Handle filter mode
	if state.filterMode {
		switch ev.Key {
		// FIX 1: Handle Ctrl-C inside Filter Mode to allow exit
		case termbox.KeyCtrlC:
			if *currentCmd != nil {
				(*currentCmd).Process.Kill()
			}
			return true

		case termbox.KeyEsc:
			// Exit filter mode and restore original playlist
			if state.originalPlaylist != nil {
				mu.Lock()
				state.playlist = state.originalPlaylist
				*songs = state.originalPlaylist
				*songIndex = state.originalIndex
				state.totalSongs = len(state.originalPlaylist)

				// Update current song reference
				if *songIndex < len(*songs) {
					state.currentSong = &(*songs)[*songIndex]
					state.songIndex = *songIndex
				}
				mu.Unlock()

				state.originalPlaylist = nil
				state.filteredPlaylist = nil
			}
			state.filterMode = false
			state.activeWindow = state.filterType // Return to the window we filtered from
			state.message = "Filter cleared - returned to full playlist"
			return false

		case termbox.KeyEnter:
			// Apply the filter and start playback
			var filterValue string
			var scroll int

			switch state.filterType {
			case "artist":
				if state.artistScroll < len(state.artists) {
					filterValue = state.artists[state.artistScroll].Key
				}
				scroll = state.artistScroll
			case "genre":
				if state.genreScroll < len(state.genres) {
					filterValue = state.genres[state.genreScroll].Key
				}
				scroll = state.genreScroll
			case "year":
				if state.yearScroll < len(state.years) {
					filterValue = state.years[state.yearScroll].Key
				}
				scroll = state.yearScroll
			}

			if filterValue != "" {
				filtered := filterPlaylist(state.playlist, state.filterType, filterValue)

				if len(filtered) > 0 {
					mu.Lock()
					// Save original state
					state.originalPlaylist = state.playlist
					state.originalIndex = *songIndex

					// Apply filter
					state.filteredPlaylist = filtered
					state.playlist = filtered
					*songs = filtered
					*songIndex = 0
					state.songIndex = 0
					state.totalSongs = len(filtered)
					state.currentSong = &filtered[0]
					state.filterValue = filterValue

					// Kill current song if playing
					if *currentCmd != nil {
						(*currentCmd).Process.Kill()
					}

					// Start playback of filtered playlist
					if !*playbackActive {
						*playbackActive = true
						go playback(state, currentCmd, songs, songIndex, playbackActive, mu)
					}
					mu.Unlock()

					state.message = fmt.Sprintf("Playing %d songs: %s = %s (Esc to restore)", len(filtered), state.filterType, filterValue)
				} else {
					state.message = "No songs match this filter"
				}
			}
			state.filterMode = false
			state.activeWindow = state.filterType
			return false

		case termbox.KeyArrowUp:
			scrollWindow(state, -1)
			return false
		case termbox.KeyArrowDown:
			scrollWindow(state, 1)
			return false
		}
		return false
	}

	// Handle query window mode separately
	if state.activeWindow == "query" {
		switch ev.Key {
		// FIX 2: Separate Esc (switch window) from Ctrl-C (Quit app)
		case termbox.KeyCtrlC:
			if *currentCmd != nil {
				(*currentCmd).Process.Kill()
			}
			return true
		case termbox.KeyEsc:
			state.activeWindow = "artist"
			return false
		case termbox.KeyEnter:
			runQuery(collection, state, randomize, limit)
			state.activeWindow = "artist"
			mu.Lock()
			if !*playbackActive {
				*playbackActive = true
				go playback(state, currentCmd, songs, songIndex, playbackActive, mu)
			}
			mu.Unlock()
		case termbox.KeySpace:
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
			state.activeWindow = "artist"
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
	case termbox.KeyEnter:
		// NEW: Enter in a column window activates filter mode
		if len(state.playlist) > 0 {
			state.filterMode = true
			state.filterType = state.activeWindow
			state.message = fmt.Sprintf("Filter mode: Select %s and press Enter to play, Esc to cancel", state.activeWindow)
		}
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

// Modified drawUI to show filter mode
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

	// Show filter status if active
	if state.originalPlaylist != nil {
		filterStatus := fmt.Sprintf(" [FILTERED: %s=%s]", state.filterType, state.filterValue)
		drawText(queryX+len(state.query)+2, 0, filterStatus, termbox.ColorMagenta|termbox.AttrBold, termbox.ColorDefault)
	}

	if state.activeWindow == "query" {
		termbox.SetCursor(queryX+state.cursorPos, 0)
	} else {
		termbox.HideCursor()
	}

	// Draw separator
	for x := 0; x < w; x++ {
		termbox.SetCell(x, 1, '─', termbox.ColorWhite, termbox.ColorDefault)
	}

	songInfoHeight := 6
	middleHeight := (h - 2 - songInfoHeight - 4) / 2
	if middleHeight < 5 {
		middleHeight = 5
	}
	queueHeight := h - 2 - middleHeight - songInfoHeight - 3
	if queueHeight < 3 {
		queueHeight = 3
	}

	middleY := 2
	queueY := middleY + middleHeight + 1
	bottomY := queueY + queueHeight + 1

	// Middle: Three columns
	colWidth := w / 3
	drawColumn(0, middleY, colWidth, middleHeight, "Artists (a)", state.artists, state.artistScroll, state.activeWindow == "artist", state.filterMode && state.filterType == "artist")
	drawColumn(colWidth, middleY, colWidth, middleHeight, "Genres (g)", state.genres, state.genreScroll, state.activeWindow == "genre", state.filterMode && state.filterType == "genre")
	drawColumn(colWidth*2, middleY, w-colWidth*2, middleHeight, "Years (y)", state.years, state.yearScroll, state.activeWindow == "year", state.filterMode && state.filterType == "year")

	// Draw separator before queue section
	for x := 0; x < w; x++ {
		termbox.SetCell(x, queueY-1, '─', termbox.ColorWhite, termbox.ColorDefault)
	}

	drawQueue(0, queueY, w, queueHeight, state)

	// Draw separator before bottom
	for x := 0; x < w; x++ {
		termbox.SetCell(x, bottomY-1, '─', termbox.ColorWhite, termbox.ColorDefault)
	}

	drawSongInfo(0, bottomY, w, state)

	// Status line at very bottom
	statusY := h - 1
	var status string
	if state.filterMode {
		status = fmt.Sprintf(" [↑↓]=Select [Enter]=Apply Filter [Esc]=Cancel | %s", state.message)
	} else if state.originalPlaylist != nil {
		status = fmt.Sprintf(" [Enter]=Filter [Esc]=Clear Filter [s]=Skip [q]=Quit [?]=Help | %s", state.message)
	} else {
		status = fmt.Sprintf(" [Space]=Query [Enter]=Filter [↑↓]=Scroll [s]=Skip [q]=Quit [?]=Help | %s", state.message)
	}
	drawText(0, statusY, status, termbox.ColorBlack, termbox.ColorWhite)

	termbox.Flush()
}

// Modified drawColumn to highlight filter mode
func drawColumn(x, y, w, h int, title string, items []CountItem, scroll int, active bool, filtering bool) {
	fg := termbox.ColorWhite
	if filtering {
		fg = termbox.ColorMagenta | termbox.AttrBold
	} else if active {
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

		// Highlight selected item in filter mode
		itemFg := termbox.ColorWhite
		itemBg := termbox.ColorDefault
		if filtering && scroll+i == scroll {
			itemFg = termbox.ColorBlack
			itemBg = termbox.ColorMagenta
		}

		drawTextWithBg(x, y+1+i, line, itemFg, itemBg)
	}

	// Show scroll indicators
	if scroll > 0 {
		drawText(x+w-3, y, "▲", termbox.ColorCyan, termbox.ColorDefault)
	}
	if scroll+visibleHeight < len(items) {
		drawText(x+w-3, y+h-1, "▼", termbox.ColorCyan, termbox.ColorDefault)
	}
}

// Helper function for drawing text with background
func drawTextWithBg(x, y int, text string, fg, bg termbox.Attribute) {
	for i, ch := range text {
		termbox.SetCell(x+i, y, ch, fg, bg)
	}
}

// Updated help text
func drawHelp(w, h int) {
	help := []string{
		"DeeJay TUI - Keyboard Commands",
		"",
		"  Space   - Jump to query window",
		"  Enter   - Execute query / Filter selected item",
		"  Esc     - Exit mode / Clear filter / Quit",
		"  q       - Quit (when not in query window)",
		"  s       - Skip current song",
		"  a       - Switch to Artists window",
		"  g       - Switch to Genres window",
		"  y       - Switch to Years window",
		"  ↑/↓     - Scroll in active window",
		"  h or ?  - Show this help",
		"",
		"Filtering:",
		"  1. Navigate to artist/genre/year window",
		"  2. Use ↑/↓ to select item",
		"  3. Press Enter to filter and play",
		"  4. Press Esc to restore full playlist",
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