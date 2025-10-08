package main

import (
	"crypto/sha1"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dhowden/tag"
	"github.com/faiface/beep/flac"
	beepmp3 "github.com/faiface/beep/mp3"
	"github.com/faiface/beep/vorbis"
	"github.com/faiface/beep/wav"
	"github.com/hajimehoshi/go-mp3"
)

// FileMeta holds the metadata we extract from an audio file.
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
	Length   float64 // seconds
	Bitrate  int     // kbps
	Sample   int     // sample rate in Hz
	Format   string
	Size     int64
	ModTime  string
	Checksum string
}

// dummyMetadata implements github.com/dhowden/tag.Metadata with zero values
type dummyMetadata struct{}

func (d *dummyMetadata) Format() tag.Format          { return "" }
func (d *dummyMetadata) FileType() tag.FileType      { return tag.UnknownFileType }
func (d *dummyMetadata) Title() string               { return "" }
func (d *dummyMetadata) Album() string               { return "" }
func (d *dummyMetadata) Artist() string              { return "" }
func (d *dummyMetadata) AlbumArtist() string         { return "" }
func (d *dummyMetadata) Composer() string            { return "" }
func (d *dummyMetadata) Genre() string               { return "" }
func (d *dummyMetadata) Year() int                   { return 0 }
func (d *dummyMetadata) Track() (int, int)           { return 0, 0 }
func (d *dummyMetadata) Disc() (int, int)            { return 0, 0 }
func (d *dummyMetadata) Picture() *tag.Picture       { return nil }
func (d *dummyMetadata) Lyrics() string              { return "" }
func (d *dummyMetadata) Comment() string             { return "" }
func (d *dummyMetadata) Raw() map[string]interface{} { return nil }

// processFile extracts metadata from a single audio file.
func processFile(path string) *FileMeta {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot open %s: %v\n", path, err)
		return nil
	}
	defer f.Close()

	// Compute SHA1 checksum
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		fmt.Fprintf(os.Stderr, "Checksum failed for %s: %v\n", path, err)
		return nil
	}
	sum := fmt.Sprintf("%x", h.Sum(nil))

	// Re-open to read metadata
	f.Seek(0, io.SeekStart)
	m, err := tag.ReadFrom(f)
	if err != nil || m == nil {
		m = &dummyMetadata{}
	}

	info, err := f.Stat()
	if err != nil {
		return nil
	}

	format := fmt.Sprintf("%T", m)
	ext := strings.ToLower(filepath.Ext(path))

	track, _ := m.Track()
	disc, _ := m.Disc()

	var lengthSec float64
	var sampleRate, bitrate int

	// --- Determine length, sample rate, and bitrate ---
	switch ext {
	case ".mp3":
		f.Seek(0, io.SeekStart)
		streamer, formatData, err := beepmp3.Decode(f)
		if err == nil {
			defer streamer.Close()
			lengthSec = float64(streamer.Len()) / float64(formatData.SampleRate)
			sampleRate = int(formatData.SampleRate)
			if info.Size() > 0 && lengthSec > 0 {
				bitrate = int(float64(info.Size()*8) / lengthSec / 1000)
			}
		} else {
			// fallback decoder
			f.Seek(0, io.SeekStart)
			dec, err2 := mp3.NewDecoder(f)
			if err2 == nil {
				sampleRate = 44100                                        // fallback assumption
				lengthSec = float64(dec.Length()) / float64(sampleRate*4) // 16-bit stereo
				if info.Size() > 0 && lengthSec > 0 {
					bitrate = int(float64(info.Size()*8) / lengthSec / 1000)
				}
			}
		}

	case ".flac":
		f.Seek(0, io.SeekStart)
		streamer, formatData, err := flac.Decode(f)
		if err != nil {
			break
		}
		defer streamer.Close()
		lengthSec = float64(streamer.Len()) / float64(formatData.SampleRate)
		sampleRate = int(formatData.SampleRate)
		if info.Size() > 0 && lengthSec > 0 {
			bitrate = int(float64(info.Size()*8) / lengthSec / 1000)
		}

	case ".wav":
		f.Seek(0, io.SeekStart)
		streamer, formatData, err := wav.Decode(f)
		if err != nil {
			break
		}
		defer streamer.Close()
		lengthSec = float64(streamer.Len()) / float64(formatData.SampleRate)
		sampleRate = int(formatData.SampleRate)
		if info.Size() > 0 && lengthSec > 0 {
			bitrate = int(float64(info.Size()*8) / lengthSec / 1000)
		}

	case ".ogg":
		f.Seek(0, io.SeekStart)
		streamer, formatData, err := vorbis.Decode(f)
		if err != nil {
			break
		}
		defer streamer.Close()
		lengthSec = float64(streamer.Len()) / float64(formatData.SampleRate)
		sampleRate = int(formatData.SampleRate)
		if info.Size() > 0 && lengthSec > 0 {
			bitrate = int(float64(info.Size()*8) / lengthSec / 1000)
		}

	default:
		lengthSec, sampleRate, bitrate = 0, 0, 0
	}

	return &FileMeta{
		Path:     path,
		Ext:      ext,
		Artist:   m.Artist(),
		Album:    m.Album(),
		Title:    m.Title(),
		Genre:    m.Genre(),
		Year:     m.Year(),
		Track:    track,
		Disc:     disc,
		Length:   lengthSec,
		Bitrate:  bitrate,
		Sample:   sampleRate,
		Format:   format,
		Size:     info.Size(),
		ModTime:  info.ModTime().Format(time.RFC3339),
		Checksum: sum,
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: DeeJay <music-dir>")
		return
	}
	root := os.Args[1]

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil // skip dirs and problems
		}
		meta := processFile(path)
		if meta != nil {
			fmt.Printf("%+v\n", meta)
		}
		return nil
	})
}
