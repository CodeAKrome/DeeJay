package main

import (
	"crypto/sha1"
	"encoding/csv"
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
)

var audioExts = map[string]bool{
	".mp3":  true,
	".flac": true,
	".aif":  true,
	".aiff": true,
	".m4a":  true,
	".ogg":  true,
	".wav":  true,
	".wma":  true,
	".ape":  true,
	".mpc":  true,
	".wv":   true,
	".m4r":  true,
}

type FileMeta struct {
	Path     string
	Ext      string
	Artist   string
	Album    string
	Title    string
	Genre    string
	Year     int
	Track    int
	Disc     int
	Length   float64
	Bitrate  int
	Sample   int
	Format   string
	Size     int64
	ModTime  string
	Checksum string
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: audio_inventory <directory>")
		os.Exit(1)
	}
	root := os.Args[1]

	outFile, err := os.Create("audio_inventory.tsv")
	if err != nil {
		log.Fatalf("Cannot create output file: %v", err)
	}
	defer outFile.Close()

	dupFile, err := os.Create("duplicates.tsv")
	if err != nil {
		log.Fatalf("Cannot create duplicates file: %v", err)
	}
	defer dupFile.Close()

	writer := csv.NewWriter(outFile)
	writer.Comma = '\t'
	writer.Write([]string{
		"Path", "Extension", "Artist", "Album", "Title",
		"Genre", "Year", "Track", "Disc", "DurationSeconds",
		"Bitrate", "SampleRate", "Format", "FileSizeBytes",
		"ModTime", "Checksum",
	})

	dupWriter := csv.NewWriter(dupFile)
	dupWriter.Comma = '\t'
	dupWriter.Write([]string{"OriginalPath", "DuplicatePath"})

	numWorkers := runtime.NumCPU() * 2
	filesChan := make(chan string, numWorkers*4)
	resultsChan := make(chan *FileMeta, numWorkers*4)

	var wg sync.WaitGroup
	var mu sync.Mutex

	checksumMap := make(map[string]string)

	// Counters for progress display
	var totalFiles, uniqueFiles, duplicateFiles int64

	// --- Worker goroutines ---
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range filesChan {
				meta := processFile(path)
				if meta == nil {
					continue
				}

				mu.Lock()
				totalFiles++
				if existing, ok := checksumMap[meta.Checksum]; ok {
					dupWriter.Write([]string{existing, meta.Path})
					duplicateFiles++
					mu.Unlock()
					continue
				}
				checksumMap[meta.Checksum] = meta.Path
				uniqueFiles++
				mu.Unlock()

				resultsChan <- meta
			}
		}()
	}

	// --- Progress Meter ---
	startTime := time.Now()
	stopMeter := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				elapsed := time.Since(startTime).Round(time.Second)
				mu.Lock()
				fmt.Printf("\r[Progress] Total: %d | Unique: %d | Duplicates: %d | Elapsed: %v ",
					totalFiles, uniqueFiles, duplicateFiles, elapsed)
				mu.Unlock()
			case <-stopMeter:
				return
			}
		}
	}()

	// --- Directory walker ---
	go func() {
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				return nil
			}
			if info.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if audioExts[ext] {
				filesChan <- path
			}
			return nil
		})
		close(filesChan)
	}()

	// --- Collector goroutine ---
	done := make(chan struct{})
	go func() {
		for meta := range resultsChan {
			writer.Write([]string{
				meta.Path, meta.Ext, meta.Artist, meta.Album, meta.Title,
				meta.Genre, fmt.Sprintf("%d", meta.Year),
				fmt.Sprintf("%d", meta.Track),
				fmt.Sprintf("%d", meta.Disc),
				fmt.Sprintf("%.2f", meta.Length),
				fmt.Sprintf("%d", meta.Bitrate),
				fmt.Sprintf("%d", meta.Sample),
				meta.Format,
				fmt.Sprintf("%d", meta.Size),
				meta.ModTime,
				meta.Checksum,
			})
		}
		done <- struct{}{}
	}()

	wg.Wait()          // all workers finished
	close(resultsChan) // signal collector
	<-done             // wait collector done

	close(stopMeter)
	fmt.Print("\n") // newline after progress meter

	writer.Flush()
	dupWriter.Flush()
	fmt.Printf("✅ Inventory complete.\n")
	fmt.Printf("  - Main inventory: audio_inventory.tsv\n")
	fmt.Printf("  - Duplicates: duplicates.tsv\n")
	fmt.Printf("  - Total files processed: %d (Unique: %d, Duplicates: %d)\n",
		totalFiles, uniqueFiles, duplicateFiles)
}

func processFile(path string) *FileMeta {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot open %s: %v\n", path, err)
		return nil
	}
	defer f.Close()

	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		fmt.Fprintf(os.Stderr, "Checksum failed for %s: %v\n", path, err)
		return nil
	}
	sum := fmt.Sprintf("%x", h.Sum(nil))

	// Reopen to read metadata
	f.Seek(0, io.SeekStart)
	m, _ := tag.ReadFrom(f)

	info, err := f.Stat()
	if err != nil {
		return nil
	}

	format := fmt.Sprintf("%T", m)

	return &FileMeta{
		Path:     path,
		Ext:      strings.ToLower(filepath.Ext(path)),
		Artist:   m.Artist(),
		Album:    m.Album(),
		Title:    m.Title(),
		Genre:    m.Genre(),
		Year:     m.Year(),
		Track:    m.Track(),
		Disc:     m.Disc(),
		Length:   m.Length(),
		Bitrate:  m.Bitrate(),
		Sample:   m.SampleRate(),
		Format:   format,
		Size:     info.Size(),
		ModTime:  info.ModTime().Format(time.RFC3339),
		Checksum: sum,
	}
}
