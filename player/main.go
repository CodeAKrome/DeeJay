func queryMusic(cliQuery string, randomize bool) {
	collection := connectToMongo()

	// Write PID file for external control
	if err := ioutil.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
		log.Printf("Warning: Could not write PID file: %v", err)
	}
	defer os.Remove(pidFile)
	defer os.Remove(commandFile)

	fmt.Println("\n=== DeeJay Music Query ===")
	if cliQuery != "" {
		fmt.Printf("Running CLI query: %s\n", cliQuery)
	}

	fmt.Println("Enter queries like:")
	fmt.Println("  - play punk from the 1980s")
	fmt.Println("  - play rock from 1995 to 2000")
	fmt.Println("  - play jazz by Miles Davis")
	fmt.Println("  - play songs from the 70s")
	fmt.Println("Type 'quit' to exit\n")

	scanner := bufio.NewScanner(os.Stdin)
	for {
		var query string
		if cliQuery != "" {
			query = cliQuery
			cliQuery = "" // consume it once
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

		// -------------- RANDOMIZE --------------
		if randomize {
			rand.Shuffle(len(songs), func(i, j int) {
				songs[i], songs[j] = songs[j], songs[i]
			})
		}
		// ---------------------------------------

		fmt.Printf("\nFound %d songs:\n", len(songs))
		for i, song := range songs {
			fmt.Printf("%d. %s - %s (%d) [%s]\n", i+1, song.Artist, song.Title, song.Year, song.Genre)
		}

		if cliQuery == "" { // interactive mode
			fmt.Printf("\nPlay all? (y/n): ")
			if !scanner.Scan() {
				break
			}
			if strings.ToLower(strings.TrimSpace(scanner.Text())) == "y" {
				playSongs(songs)
			}
		} else { // CLI mode: auto-play
			playSongs(songs)
			break
		}
		fmt.Println()
	}
}