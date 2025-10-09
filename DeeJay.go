package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
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

// ============================================================================
// Data Structures
// ============================================================================

type FileMeta struct {
	Path     string  `json:"path"`
	Ext      string  `json:"ext"`
	Artist   string  `json:"artist"`
	Album    string  `json:"album"`
	Title    string  `json:"title"`
	Genre    string  `json:"genre"`
	Year     int     `json:"year"`
	Track    int     `json:"track"`
	Disc     int     `json:"disc"`
	Length   float64 `json:"length"`
	Bitrate  int     `json:"bitrate"`
	Sample   int     `json:"sample"`
	Format   string  `json:"format"`
	Size     int64   `json:"size"`
	ModTime  string  `json:"modtime"`
	Checksum string  `json:"checksum"`
}

type AgentState struct {
	ConversationHistory []Message
	MusicLibraryPath    string
	ChromaDBPath        string
	CollectionName      string
	EmbedModel          string
	LLMModel            string
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ============================================================================
// Audio Metadata Processing
// ============================================================================

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

func processFile(path string) *FileMeta {
	f, err := os.Open(path)
	if err != nil {
		log.Printf("Cannot open %s: %v\n", path, err)
		return nil
	}
	defer f.Close()

	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		log.Printf("Checksum failed for %s: %v\n", path, err)
		return nil
	}
	sum := fmt.Sprintf("%x", h.Sum(nil))

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
			f.Seek(0, io.SeekStart)
			dec, err2 := mp3.NewDecoder(f)
			if err2 == nil {
				sampleRate = 44100
				lengthSec = float64(dec.Length()) / float64(sampleRate*4)
				if info.Size() > 0 && lengthSec > 0 {
					bitrate = int(float64(info.Size()*8) / lengthSec / 1000)
				}
			}
		}

	case ".flac":
		f.Seek(0, io.SeekStart)
		streamer, formatData, err := flac.Decode(f)
		if err == nil {
			defer streamer.Close()
			lengthSec = float64(streamer.Len()) / float64(formatData.SampleRate)
			sampleRate = int(formatData.SampleRate)
			if info.Size() > 0 && lengthSec > 0 {
				bitrate = int(float64(info.Size()*8) / lengthSec / 1000)
			}
		}

	case ".wav":
		f.Seek(0, io.SeekStart)
		streamer, formatData, err := wav.Decode(f)
		if err == nil {
			defer streamer.Close()
			lengthSec = float64(streamer.Len()) / float64(formatData.SampleRate)
			sampleRate = int(formatData.SampleRate)
			if info.Size() > 0 && lengthSec > 0 {
				bitrate = int(float64(info.Size()*8) / lengthSec / 1000)
			}
		}

	case ".ogg":
		f.Seek(0, io.SeekStart)
		streamer, formatData, err := vorbis.Decode(f)
		if err == nil {
			defer streamer.Close()
			lengthSec = float64(streamer.Len()) / float64(formatData.SampleRate)
			sampleRate = int(formatData.SampleRate)
			if info.Size() > 0 && lengthSec > 0 {
				bitrate = int(float64(info.Size()*8) / lengthSec / 1000)
			}
		}
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

// ============================================================================
// ChromaDB Integration
// ============================================================================

func initChromaDB(dbPath, collectionName string) error {
	pythonCode := fmt.Sprintf(`
import chromadb
from chromadb.config import Settings
import sys

try:
    client = chromadb.PersistentClient(
        path="%s",
        settings=Settings(
            anonymized_telemetry=False,
            allow_reset=True
        )
    )
    
    collection = client.get_or_create_collection(
        name="%s",
        metadata={"hnsw:space": "cosine"}
    )
    
    print(f"ChromaDB initialized at {client._settings.persist_directory}")
    print(f"Collection '{collection.name}' ready with {collection.count()} documents")
    sys.exit(0)
    
except Exception as e:
    print(f"ChromaDB init error: {e}", file=sys.stderr)
    sys.exit(1)
`, dbPath, collectionName)

	cmd := exec.Command("python3", "-c", pythonCode)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ChromaDB initialization failed: %v - %s\n\nMake sure chromadb is installed:\n  pip install chromadb", err, string(out))
	}

	log.Println(string(out))
	return nil
}

func addToChromaDB(dbPath, collectionName string, meta *FileMeta, embedModel string) error {
	doc := fmt.Sprintf("Artist: %s, Album: %s, Title: %s, Genre: %s, Year: %d, Path: %s",
		meta.Artist, meta.Album, meta.Title, meta.Genre, meta.Year, meta.Path)

	docFile, err := os.CreateTemp("", "doc_*.txt")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %v", err)
	}
	defer os.Remove(docFile.Name())

	if _, err := docFile.Write([]byte(doc)); err != nil {
		docFile.Close()
		return fmt.Errorf("failed to write temp file: %v", err)
	}
	docFile.Close()

	metadataJSON := map[string]string{
		"path":    meta.Path,
		"artist":  meta.Artist,
		"album":   meta.Album,
		"title":   meta.Title,
		"genre":   meta.Genre,
		"year":    fmt.Sprintf("%d", meta.Year),
		"length":  fmt.Sprintf("%.2f", meta.Length),
		"bitrate": fmt.Sprintf("%d", meta.Bitrate),
	}

	metadataBytes, err := json.Marshal(metadataJSON)
	if err != nil {
		return err
	}

	metadataFile, err := os.CreateTemp("", "metadata_*.json")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %v", err)
	}
	defer os.Remove(metadataFile.Name())

	if _, err := metadataFile.Write(metadataBytes); err != nil {
		metadataFile.Close()
		return fmt.Errorf("failed to write temp file: %v", err)
	}
	metadataFile.Close()

	pythonCode := fmt.Sprintf(`
import chromadb
from chromadb.config import Settings
from sentence_transformers import SentenceTransformer
import json
import sys

try:
    client = chromadb.PersistentClient(
        path="%s",
        settings=Settings(anonymized_telemetry=False)
    )
    
    collection = client.get_or_create_collection(
        name="%s",
        metadata={"hnsw:space": "cosine"}
    )
    
    model = SentenceTransformer('%s')
    
    with open("%s", "r", encoding="utf-8") as f:
        document = f.read()
    
    with open("%s", "r", encoding="utf-8") as f:
        metadata = json.load(f)
    
    embedding = model.encode(document).tolist()
    
    collection.upsert(
        ids=["%s"],
        documents=[document],
        embeddings=[embedding],
        metadatas=[metadata]
    )
    
    print(f"Added document {len(collection.get()['ids'])} total")
    sys.exit(0)
    
except Exception as e:
    print(f"ChromaDB add error: {e}", file=sys.stderr)
    sys.exit(1)
`, dbPath, collectionName, embedModel, docFile.Name(), metadataFile.Name(), meta.Checksum)

	cmd := exec.Command("python3", "-c", pythonCode)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ChromaDB add failed: %v - %s", err, string(out))
	}

	return nil
}

func queryChromaDB(dbPath, collectionName, queryText, embedModel string, nResults int) ([]map[string]interface{}, error) {
	queryFile, err := os.CreateTemp("", "query_*.txt")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %v", err)
	}
	defer os.Remove(queryFile.Name())

	if _, err := queryFile.Write([]byte(queryText)); err != nil {
		queryFile.Close()
		return nil, fmt.Errorf("failed to write temp file: %v", err)
	}
	queryFile.Close()

	pythonCode := fmt.Sprintf(`
import chromadb
from chromadb.config import Settings
from sentence_transformers import SentenceTransformer
import json
import sys

try:
    client = chromadb.PersistentClient(
        path="%s",
        settings=Settings(anonymized_telemetry=False)
    )
    
    collection = client.get_collection(name="%s")
    
    model = SentenceTransformer('%s')
    
    with open("%s", "r", encoding="utf-8") as f:
        query = f.read()
    
    query_embedding = model.encode(query).tolist()
    
    results = collection.query(
        query_embeddings=[query_embedding],
        n_results=%d,
        include=["documents", "metadatas", "distances"]
    )
    
    output = []
    if results["ids"] and len(results["ids"]) > 0:
        for i in range(len(results["ids"][0])):
            output.append({
                "id": results["ids"][0][i],
                "document": results["documents"][0][i],
                "metadata": results["metadatas"][0][i],
                "distance": results["distances"][0][i]
            })
    
    print(json.dumps(output))
    sys.exit(0)
    
except Exception as e:
    print(f"ChromaDB query error: {e}", file=sys.stderr)
    sys.exit(1)
`, dbPath, collectionName, embedModel, queryFile.Name(), nResults)

	cmd := exec.Command("python3", "-c", pythonCode)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ChromaDB query failed: %v - %s", err, string(out))
	}

	var results []map[string]interface{}
	if err := json.Unmarshal(out, &results); err != nil {
		return nil, fmt.Errorf("failed to parse results: %v - output: %s", err, string(out))
	}

	return results, nil
}

func getChromaDBCount(dbPath, collectionName string) (int, error) {
	pythonCode := fmt.Sprintf(`
import chromadb
from chromadb.config import Settings
import sys

try:
    client = chromadb.PersistentClient(
        path="%s",
        settings=Settings(anonymized_telemetry=False)
    )
    
    collection = client.get_collection(name="%s")
    print(collection.count())
    sys.exit(0)
    
except Exception as e:
    print("0")
    sys.exit(0)
`, dbPath, collectionName)

	cmd := exec.Command("python3", "-c", pythonCode)
	out, err := cmd.Output()
	if err != nil {
		return 0, nil
	}

	var count int
	fmt.Sscanf(string(out), "%d", &count)
	return count, nil
}

// ============================================================================
// STT with Faster Whisper
// ============================================================================

func transcribeAudio(audioPath, modelSize string) (string, error) {
	tmpfile, err := os.CreateTemp("", "audio_path_*.txt")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.Write([]byte(audioPath)); err != nil {
		tmpfile.Close()
		return "", fmt.Errorf("failed to write temp file: %v", err)
	}
	tmpfile.Close()

	pythonCode := fmt.Sprintf(`
from faster_whisper import WhisperModel
import sys

try:
    model = WhisperModel("%s", device="cpu", compute_type="int8")
    with open("%s", "r", encoding="utf-8") as f:
        audio_path = f.read().strip()
    segments, info = model.transcribe(audio_path, beam_size=5)
    text = " ".join([segment.text for segment in segments])
    print(text.strip())
except Exception as e:
    print(f"Error: {e}", file=sys.stderr)
    sys.exit(1)
`, modelSize, tmpfile.Name())

	cmd := exec.Command("python3", "-c", pythonCode)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("transcription failed: %v - %s", err, string(out))
	}

	return strings.TrimSpace(string(out)), nil
}

// ============================================================================
// TTS with Kokoro
// ============================================================================

func synthesizeSpeech(text, outputPath, voice string) error {
	textFile, err := os.CreateTemp("", "tts_text_*.txt")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %v", err)
	}
	defer os.Remove(textFile.Name())

	if _, err := textFile.Write([]byte(text)); err != nil {
		textFile.Close()
		return fmt.Errorf("failed to write temp file: %v", err)
	}
	textFile.Close()

	outFile, err := os.CreateTemp("", "tts_output_*.txt")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %v", err)
	}
	defer os.Remove(outFile.Name())

	if _, err := outFile.Write([]byte(outputPath)); err != nil {
		outFile.Close()
		return fmt.Errorf("failed to write temp file: %v", err)
	}
	outFile.Close()

	pythonCode := fmt.Sprintf(`
import sys

try:
    from kokoro_onnx import Kokoro
    
    with open("%s", "r", encoding="utf-8") as f:
        text = f.read()
    with open("%s", "r", encoding="utf-8") as f:
        output_path = f.read().strip()
    
    kokoro = Kokoro("%s", lang="en-us")
    samples, sample_rate = kokoro.create(text)
    
    import scipy.io.wavfile as wav
    wav.write(output_path, sample_rate, samples)
    
    print("Speech synthesis complete")
except Exception as e:
    print(f"TTS Error: {e}", file=sys.stderr)
    sys.exit(1)
`, textFile.Name(), outFile.Name(), voice)

	cmd := exec.Command("python3", "-c", pythonCode)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("TTS failed: %v - %s", err, string(output))
	}

	return nil
}

// ============================================================================
// Local LLM via Ollama
// ============================================================================

func callOllamaLocal(model string, messages []Message) (string, error) {
	var prompt strings.Builder

	for _, msg := range messages {
		if msg.Role == "system" {
			prompt.WriteString(fmt.Sprintf("System: %s\n\n", msg.Content))
		} else if msg.Role == "user" {
			prompt.WriteString(fmt.Sprintf("User: %s\n\n", msg.Content))
		} else if msg.Role == "assistant" {
			prompt.WriteString(fmt.Sprintf("Assistant: %s\n\n", msg.Content))
		}
	}

	prompt.WriteString("Assistant: ")

	tmpfile, err := os.CreateTemp("", "ollama_prompt_*.txt")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.Write([]byte(prompt.String())); err != nil {
		tmpfile.Close()
		return "", fmt.Errorf("failed to write temp file: %v", err)
	}
	tmpfile.Close()

	cmd := exec.Command("ollama", "run", model)
	cmd.Stdin = strings.NewReader(prompt.String())

	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ollama failed: %v - %s\n\nMake sure Ollama is installed and model '%s' is pulled:\n  ollama pull %s",
			err, string(out), model, model)
	}

	return strings.TrimSpace(string(out)), nil
}

// ============================================================================
// Agent Core
// ============================================================================

type MusicAgent struct {
	State AgentState
}

func NewMusicAgent(musicPath, chromaDBPath, collectionName, embedModel, llmModel string) (*MusicAgent, error) {
	if err := initChromaDB(chromaDBPath, collectionName); err != nil {
		return nil, err
	}

	return &MusicAgent{
		State: AgentState{
			ConversationHistory: []Message{},
			MusicLibraryPath:    musicPath,
			ChromaDBPath:        chromaDBPath,
			CollectionName:      collectionName,
			EmbedModel:          embedModel,
			LLMModel:            llmModel,
		},
	}, nil
}

func (a *MusicAgent) IndexLibrary() error {
	log.Println("🎵 Indexing music library...")

	count := 0

	err := filepath.Walk(a.State.MusicLibraryPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".mp3" && ext != ".flac" && ext != ".wav" && ext != ".ogg" {
			return nil
		}

		meta := processFile(path)
		if meta == nil {
			return nil
		}

		if err := addToChromaDB(a.State.ChromaDBPath, a.State.CollectionName, meta, a.State.EmbedModel); err != nil {
			log.Printf("Failed to add %s to ChromaDB: %v\n", path, err)
			return nil
		}

		count++
		if count%10 == 0 {
			log.Printf("Indexed %d files...\n", count)
		}

		return nil
	})

	if err != nil {
		return err
	}

	totalCount, _ := getChromaDBCount(a.State.ChromaDBPath, a.State.CollectionName)
	log.Printf("✅ Indexed %d new audio files\n", count)
	log.Printf("📚 Total documents in ChromaDB: %d\n", totalCount)
	log.Printf("📁 ChromaDB location: %s\n", a.State.ChromaDBPath)
	return nil
}

func (a *MusicAgent) Search(query string, n int) ([]map[string]interface{}, error) {
	return queryChromaDB(a.State.ChromaDBPath, a.State.CollectionName, query, a.State.EmbedModel, n)
}

func (a *MusicAgent) ProcessVoiceQuery(audioPath, whisperModel string) (string, error) {
	log.Println("🎤 Transcribing audio...")
	text, err := transcribeAudio(audioPath, whisperModel)
	if err != nil {
		return "", err
	}

	log.Printf("📝 Transcribed: %s\n", text)

	response, err := a.ProcessTextQuery(text)
	if err != nil {
		return "", err
	}

	return response, nil
}

func (a *MusicAgent) ProcessTextQuery(query string) (string, error) {
	log.Println("🔍 Searching music library...")
	results, err := a.Search(query, 5)
	if err != nil {
		return "", err
	}

	context := "Music Library Search Results:\n"
	for i, res := range results {
		doc := res["document"].(string)
		metadata := res["metadata"].(map[string]interface{})
		distance := res["distance"].(float64)

		context += fmt.Sprintf("\n%d. %s (Relevance: %.2f)", i+1, doc, 1.0-distance)
		if path, ok := metadata["path"].(string); ok {
			context += fmt.Sprintf("\n   File: %s", path)
		}
		context += "\n"
	}

	systemPrompt := `You are an intelligent music assistant with access to a music library. 
Your role is to help users find music, answer questions about their library, and provide recommendations.
Use the search results provided to give accurate, helpful responses.
Be conversational and friendly. Keep responses concise but informative.`

	messages := []Message{
		{Role: "system", Content: systemPrompt},
	}

	messages = append(messages, a.State.ConversationHistory...)

	userMessage := fmt.Sprintf("Context:\n%s\n\nUser Query: %s", context, query)
	messages = append(messages, Message{
		Role:    "user",
		Content: userMessage,
	})

	log.Println("🤖 Generating response with local LLM...")
	response, err := callOllamaLocal(a.State.LLMModel, messages)
	if err != nil {
		return "", err
	}

	a.State.ConversationHistory = append(a.State.ConversationHistory, Message{
		Role:    "user",
		Content: query,
	})
	a.State.ConversationHistory = append(a.State.ConversationHistory, Message{
		Role:    "assistant",
		Content: response,
	})

	if len(a.State.ConversationHistory) > 10 {
		a.State.ConversationHistory = a.State.ConversationHistory[len(a.State.ConversationHistory)-10:]
	}

	return response, nil
}

func (a *MusicAgent) Speak(text string, outputPath string, voice string) error {
	log.Println("🔊 Synthesizing speech...")
	return synthesizeSpeech(text, outputPath, voice)
}

// ============================================================================
// Main
// ============================================================================

func main() {
	if len(os.Args) < 2 {
		fmt.Println("MusicRAG - Fully Local Agentic Music Assistant with ChromaDB")
		fmt.Println("\nUsage:")
		fmt.Println("  MusicRAG index <music-dir>           - Index music library")
		fmt.Println("  MusicRAG query <text>                - Text query")
		fmt.Println("  MusicRAG voice <audio-file>          - Voice query")
		fmt.Println("  MusicRAG chat                        - Interactive chat mode")
		fmt.Println("\nEnvironment Variables:")
		fmt.Println("  CHROMA_DB_PATH    - ChromaDB storage path (default: ./chroma_data)")
		fmt.Println("  COLLECTION_NAME   - Collection name (default: music_library)")
		fmt.Println("  EMBED_MODEL       - Embedding model (default: BAAI/bge-small-en-v1.5)")
		fmt.Println("  OLLAMA_MODEL      - Ollama model (default: llama3.2)")
		fmt.Println("  WHISPER_MODEL     - Whisper model size (default: base)")
		fmt.Println("  KOKORO_VOICE      - Kokoro voice (default: af_sky)")
		fmt.Println("\nSetup:")
		fmt.Println("  1. Install Ollama: curl -fsSL https://ollama.com/install.sh | sh")
		fmt.Println("  2. Pull model: ollama pull llama3.2")
		fmt.Println("  3. Install Python: pip install chromadb sentence-transformers faster-whisper kokoro-onnx scipy")
		return
	}

	chromaDBPath := os.Getenv("CHROMA_DB_PATH")
	if chromaDBPath == "" {
		chromaDBPath = "./chroma_data"
	}

	collectionName := os.Getenv("COLLECTION_NAME")
	if collectionName == "" {
		collectionName = "music_library"
	}

	embedModel := os.Getenv("EMBED_MODEL")
	if embedModel == "" {
		embedModel = "BAAI/bge-small-en-v1.5"
	}

	ollamaModel := os.Getenv("OLLAMA_MODEL")
	if ollamaModel == "" {
		ollamaModel = "llama3.2"
	}

	whisperModel := os.Getenv("WHISPER_MODEL")
	if whisperModel == "" {
		whisperModel = "base"
	}

	kokoroVoice := os.Getenv("KOKORO_VOICE")
	if kokoroVoice == "" {
		kokoroVoice = "af_sky"
	}

	cmd := os.Args[1]

	switch cmd {
	case "index":
		if len(os.Args) < 3 {
			log.Fatal("Usage: MusicRAG index <music-dir>")
		}

		agent, err := NewMusicAgent(os.Args[2], chromaDBPath, collectionName, embedModel, ollamaModel)
		if err != nil {
			log.Fatalf("❌ Failed to create agent: %v", err)
		}

		if err := agent.IndexLibrary(); err != nil {
			log.Fatalf("❌ Indexing failed: %v", err)
		}

	case "query":
		if len(os.Args) < 3 {
			log.Fatal("Usage: MusicRAG query <text>")
		}

		agent, err := NewMusicAgent("", chromaDBPath, collectionName, embedModel, ollamaModel)
		if err != nil {
			log.Fatalf("❌ Failed to create agent: %v", err)
		}

		response, err := agent.ProcessTextQuery(strings.Join(os.Args[2:], " "))
		if err != nil {
			log.Fatalf("❌ Query failed: %v", err)
		}

		fmt.Println("\n🎵 Assistant:", response)

	case "voice":
		if len(os.Args) < 3 {
			log.Fatal("Usage: MusicRAG voice <audio-file>")
		}

		agent, err := NewMusicAgent("", chromaDBPath, collectionName, embedModel, ollamaModel)
		if err != nil {
			log.Fatalf("❌ Failed to create agent: %v", err)
		}

		response, err := agent.ProcessVoiceQuery(os.Args[2], whisperModel)
		if err != nil {
			log.Fatalf("❌ Voice query failed: %v", err)
		}

		fmt.Println("\n🎵 Assistant:", response)

		outputPath := "response.wav"
		if err := agent.Speak(response, outputPath, kokoroVoice); err != nil {
			log.Printf("⚠️ TTS failed: %v", err)
		} else {
			fmt.Printf("🔊 Response saved to %s\n", outputPath)
		}

	case "chat":
		agent, err := NewMusicAgent("", chromaDBPath, collectionName, embedModel, ollamaModel)
		if err != nil {
			log.Fatalf("❌ Failed to create agent: %v", err)
		}

		totalCount, _ := getChromaDBCount(chromaDBPath, collectionName)

		fmt.Println("🎵 MusicRAG Interactive Chat")
		fmt.Println("Commands: 'exit' to quit, 'speak' to hear last response")
		fmt.Println("ChromaDB:", chromaDBPath)
		fmt.Printf("Total songs indexed: %d\n\n", totalCount)

		var lastResponse string
		scanner := bufio.NewScanner(os.Stdin)

		for {
			fmt.Print("You: ")
			if !scanner.Scan() {
				break
			}

			input := strings.TrimSpace(scanner.Text())

			if input == "" {
				continue
			}

			if input == "exit" {
				fmt.Println("👋 Goodbye!")
				break
			}

			if input == "speak" {
				if lastResponse == "" {
					fmt.Println("⚠️ No response to speak")
					continue
				}
				outputPath := fmt.Sprintf("response_%d.wav", time.Now().Unix())
				if err := agent.Speak(lastResponse, outputPath, kokoroVoice); err != nil {
					log.Printf("⚠️ TTS failed: %v\n", err)
				} else {
					fmt.Printf("🔊 Saved to %s\n\n", outputPath)
				}
				continue
			}

			response, err := agent.ProcessTextQuery(input)
			if err != nil {
				log.Printf("❌ Error: %v\n\n", err)
				continue
			}

			lastResponse = response
			fmt.Printf("\n🎵 Assistant: %s\n\n", response)
		}

	default:
		log.Fatal("❌ Unknown command. Use: index, query, voice, or chat")
	}
}
