package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
)

//go:embed web/*
var webFS embed.FS

type Config struct {
	Directories  []string `json:"directories"`
	Workers      int      `json:"workers"`
	RamDiskPath  string   `json:"ramdisk_path"`
	VolumeBoost  float64  `json:"volume_boost"`
	SonarrURL    string   `json:"sonarr_url"`
	SonarrAPIKey string   `json:"sonarr_api_key"`
	RadarrURL    string   `json:"radarr_url"`
	RadarrAPIKey string   `json:"radarr_api_key"`
	JellyfinURL    string   `json:"jellyfin_url"`
	JellyfinAPIKey string   `json:"jellyfin_api_key"`
}

var config = Config{
	Directories: []string{},
	Workers:     2,
	RamDiskPath: "/dev/shm/unmanic-go-ramdisk",
	VolumeBoost: 1.5,
	SonarrURL:    "http://localhost:8989",
	SonarrAPIKey: "",
	RadarrURL:    "http://localhost:7878",
	RadarrAPIKey: "",
	JellyfinURL:    "http://localhost:8096",
	JellyfinAPIKey: "",
}

func ffmpegPath() string {
	return "./ffmpeg"
}

func ffprobePath() string {
	return "./ffprobe"
}

func cleanRamdisk() {
	entries, err := os.ReadDir(config.RamDiskPath)
	if err != nil {
		return
	}
	for _, e := range entries {
		os.Remove(filepath.Join(config.RamDiskPath, e.Name()))
	}
}

type FileRecord struct {
	ID           string    `json:"id"`
	Path         string    `json:"path"`
	Size         int64     `json:"size"`
	Status       string    `json:"status"`
	OriginalSize int64     `json:"original_size"`
	OutputSize   int64     `json:"output_size"`
	Error        string    `json:"error"`
	UpdatedAt    time.Time `json:"updated_at"`
}

var db *sql.DB
var mu sync.Mutex
var workersRunning int
var cancelCh chan struct{}
var cancelClosed bool

func main() {
	var err error
	db, err = sql.Open(sqlite.DriverName, "./unmanic.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	initDB()
	loadConfig()
	cleanRamdisk()

	http.HandleFunc("/api/dirs", cors(corsMethod(corsAuth(handleDirs))))
	http.HandleFunc("/api/files", cors(corsMethod(corsAuth(handleFiles))))
	http.HandleFunc("/api/config", cors(corsMethod(corsAuth(handleConfig))))
	http.HandleFunc("/api/start", cors(corsMethod(corsAuth(handleStart))))
	http.HandleFunc("/api/stop", cors(corsMethod(corsAuth(handleStop))))
	http.HandleFunc("/api/status", cors(corsMethod(corsAuth(handleStatus))))
	http.HandleFunc("/api/logs", cors(corsMethod(corsAuth(handleLogs))))
	http.HandleFunc("/", serveWeb)

	log.Println("Starting unmanic-go on 0.0.0.0:8080")
	log.Fatal(http.ListenAndServe("0.0.0.0:8080", nil))
}

func initDB() {
	db.Exec(`CREATE TABLE IF NOT EXISTS files (
		id TEXT PRIMARY KEY,
		path TEXT UNIQUE,
		size INTEGER,
		status TEXT,
		original_size INTEGER,
		output_size INTEGER,
		error TEXT,
		updated_at DATETIME
	)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT
	)`)
}

func loadConfig() {
	var raw string
	db.QueryRow("SELECT value FROM settings WHERE key='config'").Scan(&raw)
	if raw != "" {
		json.Unmarshal([]byte(raw), &config)
	}
}

func saveConfig() {
	raw, _ := json.Marshal(config)
	db.Exec("INSERT OR REPLACE INTO settings (key, value) VALUES ('config', ?)", raw)
}

func cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func corsMethod(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r)
	}
}

func corsAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r)
	}
}

func serveWeb(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/" {
		path = "/index.html"
	}
	data, err := webFS.ReadFile("web" + path)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if strings.HasSuffix(path, ".js") {
		w.Header().Set("Content-Type", "application/javascript")
	} else if strings.HasSuffix(path, ".css") {
		w.Header().Set("Content-Type", "text/css")
	}
	w.Write(data)
}

func handleDirs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		json.NewEncoder(w).Encode(config.Directories)
	case "POST":
		var dirs []string
		json.NewDecoder(r.Body).Decode(&dirs)
		config.Directories = dirs
		saveConfig()
		w.WriteHeader(http.StatusOK)
	case "DELETE":
		var dirs []string
		json.NewDecoder(r.Body).Decode(&dirs)
		// Remove dirs not in request
		var newDirs []string
		for _, d := range config.Directories {
			found := false
			for _, nd := range dirs {
				if d == nd {
					found = true
					break
				}
			}
			if !found {
				newDirs = append(newDirs, d)
			}
		}
		config.Directories = newDirs
		saveConfig()
		w.WriteHeader(http.StatusOK)
	}
}

func handleFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	scanNow := r.URL.Query().Get("scan") == "1"
	if scanNow {
		go scanAll()
	}
	var files []FileRecord
	rows, _ := db.Query("SELECT id, path, size, status, original_size, output_size, error, updated_at FROM files ORDER BY updated_at DESC")
	for rows.Next() {
		var f FileRecord
		rows.Scan(&f.ID, &f.Path, &f.Size, &f.Status, &f.OriginalSize, &f.OutputSize, &f.Error, &f.UpdatedAt)
		files = append(files, f)
	}
	rows.Close()
	json.NewEncoder(w).Encode(files)
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"workers":      config.Workers,
			"ramdisk":      config.RamDiskPath,
			"volume_boost": config.VolumeBoost,
			"sonarr_url":   config.SonarrURL,
			"sonarr_key":   config.SonarrAPIKey,
			"radarr_url":   config.RadarrURL,
			"radarr_key":   config.RadarrAPIKey,
			"jellyfin_url": config.JellyfinURL,
			"jellyfin_key": config.JellyfinAPIKey,
		})
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body map[string]interface{}
	json.NewDecoder(r.Body).Decode(&body)
	if v, ok := body["workers"].(float64); ok {
		config.Workers = int(v)
	}
	if v, ok := body["volume_boost"].(float64); ok {
		config.VolumeBoost = v
	}
	if v, ok := body["sonarr_url"].(string); ok {
		config.SonarrURL = v
	}
	if v, ok := body["sonarr_key"].(string); ok {
		config.SonarrAPIKey = v
	}
	if v, ok := body["radarr_url"].(string); ok {
		config.RadarrURL = v
	}
	if v, ok := body["radarr_key"].(string); ok {
		config.RadarrAPIKey = v
	}
	if v, ok := body["jellyfin_url"].(string); ok {
		config.JellyfinURL = v
	}
	if v, ok := body["jellyfin_key"].(string); ok {
		config.JellyfinAPIKey = v
	}
	saveConfig()
	w.WriteHeader(http.StatusOK)
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if workersRunning > 0 {
		w.WriteHeader(http.StatusConflict)
		return
	}
	var req struct {
		Workers int `json:"workers"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Workers < 1 || req.Workers > 4 {
		req.Workers = 2
	}
	config.Workers = req.Workers
	saveConfig()
	workersRunning = config.Workers
	cancelCh = make(chan struct{})
	cancelClosed = false
	go runWorkers(cancelCh)
	w.WriteHeader(http.StatusOK)
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if workersRunning == 0 {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if !cancelClosed {
		close(cancelCh)
		cancelClosed = true
	}
	workersRunning = 0
	w.WriteHeader(http.StatusOK)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	var total, pending, done, errs, skipped int64
	db.QueryRow("SELECT COUNT(*) FROM files").Scan(&total)
	db.QueryRow("SELECT COUNT(*) FROM files WHERE status='pending'").Scan(&pending)
	db.QueryRow("SELECT COUNT(*) FROM files WHERE status='done'").Scan(&done)
	db.QueryRow("SELECT COUNT(*) FROM files WHERE status='error'").Scan(&errs)
	db.QueryRow("SELECT COUNT(*) FROM files WHERE status='skipped'").Scan(&skipped)
	var originalSize, outputSize int64
	db.QueryRow("SELECT COALESCE(SUM(original_size),0) FROM files WHERE status='done'").Scan(&originalSize)
	db.QueryRow("SELECT COALESCE(SUM(output_size),0) FROM files WHERE status='done'").Scan(&outputSize)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total": total,
		"pending": pending,
		"done": done,
		"errors": errs,
		"skipped": skipped,
		"running": workersRunning,
		"original_size": originalSize,
		"output_size": outputSize,
	})
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	// Simple log fetch - in production use proper log rotation
	// For now return empty array placeholder
	json.NewEncoder(w).Encode([]string{})
}

func scanAll() {
	for _, dir := range config.Directories {
		filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if !isVideo(ext) {
				return nil
			}
			mu.Lock()
			var existing int
			db.QueryRow("SELECT COUNT(*) FROM files WHERE path=?", path).Scan(&existing)
			if existing == 0 {
				db.Exec("INSERT OR IGNORE INTO files (id, path, size, status, original_size, output_size, error, updated_at) VALUES (?, ?, ?, 'pending', ?, 0, '', ?)",
					uuid.New().String(), path, info.Size(), info.Size(), time.Now())
			}
			mu.Unlock()
			return nil
		})
	}
}

func isVideo(ext string) bool {
	switch ext {
	case ".mp4", ".mkv", ".avi", ".mov", ".wmv", ".flv", ".webm", ".m4v", ".mpg", ".mpeg", ".ts", ".vob":
		return true
	}
	return false
}

func runWorkers(cancel chan struct{}) {
	for i := 0; i < config.Workers; i++ {
		go worker(i, cancel)
	}
}

func worker(id int, cancel chan struct{}) {
	for {
		select {
		case <-cancel:
			return
		default:
		}
		var fileID, path string
		var size int64
		err := db.QueryRow("SELECT id, path, size FROM files WHERE status='pending' LIMIT 1").Scan(&fileID, &path, &size)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		processFile(fileID, path, size)
	}
}

func processFile(fileID, path string, originalSize int64) {
	updateStatus(fileID, "processing", 0, "")
	// Skip corrupt/invalid input files (e.g. zero-filled) instead of failing in ffmpeg
	if _, err := probeDuration(path); err != nil {
		updateStatus(fileID, "skipped", originalSize, "invalid input: "+err.Error())
		return
	}
	ramFile, err := copyToRamdisk(path)
	if err != nil {
		updateStatus(fileID, "error", originalSize, err.Error())
		return
	}
	defer os.Remove(ramFile)

	outputFile := ramFile + ".mp4"
	defer os.Remove(outputFile) // ensure ramdisk is freed promptly
	cmd := exec.Command(ffmpegPath(), "-y", "-i", ramFile,
		"-map", "0:v", "-c:v", "libx264", "-preset", "fast", "-crf", "23",
		"-vf", "scale=-2:720",
		"-map", "0:a:m:language:eng?", "-c:a", "aac", "-b:a", "192k",
		"-af", fmt.Sprintf("volume=%.2f", config.VolumeBoost),
		"-ac", "2",
		"-sn",
		"-f", "mp4", outputFile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Remove(outputFile)
		updateStatus(fileID, "error", originalSize, "ffmpeg: "+err.Error())
		return
	}

	if !verifyOutput(outputFile, ramFile) {
		updateStatus(fileID, "error", originalSize, "verification failed")
		os.Remove(outputFile)
		return
	}

	stat, _ := os.Stat(outputFile)
	outputSize := stat.Size()

	// Atomic replace
	tmpReplace := path + ".unmanic.tmp"
	if err := copyFile(outputFile, tmpReplace); err != nil {
		os.Remove(outputFile)
		os.Remove(tmpReplace)
		updateStatus(fileID, "error", originalSize, "copy: "+err.Error())
		return
	}
	os.Remove(path)
	if err := os.Rename(tmpReplace, path); err != nil {
		os.Remove(outputFile)
		os.Remove(tmpReplace)
		updateStatus(fileID, "error", originalSize, "rename: "+err.Error())
		return
	}
	os.Remove(outputFile)

	updateStatus(fileID, "done", outputSize, "")
	notifyLibrary(path)
}

func notifyLibrary(path string) {
	if strings.Contains(path, "/mnt/tvshows/") {
		triggerSonarrRescan(path)
	} else if strings.Contains(path, "/mnt/movies/") {
		triggerRadarrRescan(path)
	}
	triggerJellyfinRefresh()
}

func triggerSonarrRescan(path string) {
	if config.SonarrURL == "" || config.SonarrAPIKey == "" {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", config.SonarrURL+"/api/v3/series", nil)
	req.Header.Set("X-Api-Key", config.SonarrAPIKey)
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var series []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&series); err != nil {
		return
	}
	for _, ser := range series {
		seriesPath, _ := ser["path"].(string)
		if seriesPath != "" && strings.HasPrefix(path, seriesPath) {
			seriesID := int(ser["id"].(float64))
			cmdReq, _ := http.NewRequest("POST", config.SonarrURL+"/api/v3/command",
				strings.NewReader(fmt.Sprintf(`{"name":"RescanSeries","seriesId":%d}`, seriesID)))
			cmdReq.Header.Set("Content-Type", "application/json")
			cmdReq.Header.Set("X-Api-Key", config.SonarrAPIKey)
			client.Do(cmdReq)
			break
		}
	}
}

func triggerRadarrRescan(path string) {
	if config.RadarrURL == "" || config.RadarrAPIKey == "" {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("GET", config.RadarrURL+"/api/v3/movie", nil)
	req.Header.Set("X-Api-Key", config.RadarrAPIKey)
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var movies []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&movies); err != nil {
		return
	}
	for _, m := range movies {
		movieFile, _ := m["movieFile"].(map[string]interface{})
		if movieFile != nil {
			fullPath, _ := movieFile["path"].(string)
			if fullPath != "" && path == fullPath {
				movieID := int(m["id"].(float64))
				cmdReq, _ := http.NewRequest("POST", config.RadarrURL+"/api/v3/command",
					strings.NewReader(fmt.Sprintf(`{"name":"RescanMovie","movieId":%d}`, movieID)))
				cmdReq.Header.Set("Content-Type", "application/json")
				cmdReq.Header.Set("X-Api-Key", config.RadarrAPIKey)
				client.Do(cmdReq)
				break
			}
		}
	}
}

func triggerJellyfinRefresh() {
	if config.JellyfinURL == "" || config.JellyfinAPIKey == "" {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest("POST", config.JellyfinURL+"/Library/Refresh?ApiKey="+config.JellyfinAPIKey, nil)
	client.Do(req)
}


func copyToRamdisk(src string) (string, error) {
	if err := os.MkdirAll(config.RamDiskPath, 0755); err != nil {
		return "", err
	}
	dst := filepath.Join(config.RamDiskPath, filepath.Base(src)+"."+uuid.New().String())
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	out, err := os.Create(dst)
	if err != nil {
		in.Close()
		return "", err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	in.Close()
	if copyErr != nil {
		os.Remove(dst)
		return "", copyErr
	}
	if closeErr != nil {
		os.Remove(dst)
		return "", closeErr
	}
	return dst, nil
}

func verifyOutput(output, input string) bool {
	// Check duration within 1%
	inDur, err1 := probeDuration(input)
	outDur, err2 := probeDuration(output)
	if err1 != nil || err2 != nil || inDur == 0 {
		return false
	}
	diff := (outDur - inDur) / inDur
	if diff < 0 {
		diff = -diff
	}
	if diff > 0.01 {
		return false
	}

	// Check video codec
	vCodec, err := probeVideoCodec(output)
	if err != nil || !strings.EqualFold(vCodec, "h264") {
		return false
	}

	// Check audio codec
	aCodec, err := probeAudioCodec(output)
	if err != nil || !strings.EqualFold(aCodec, "aac") {
		return false
	}

	return true
}

func probeDuration(path string) (float64, error) {
	cmd := exec.Command(ffprobePath(), "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	var d float64
	fmt.Sscanf(strings.TrimSpace(string(out)), "%f", &d)
	return d, nil
}

func probeVideoCodec(path string) (string, error) {
	cmd := exec.Command(ffprobePath(), "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_name", "-of", "default=noprint_wrappers=1:nokey=1", path)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func probeAudioCodec(path string) (string, error) {
	cmd := exec.Command(ffprobePath(), "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name", "-of", "default=noprint_wrappers=1:nokey=1", path)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		in.Close()
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	in.Close()
	if copyErr != nil {
		os.Remove(dst)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(dst)
		return closeErr
	}
	return nil
}

func updateStatus(fileID, status string, outputSize int64, errStr string) {
	mu.Lock()
	defer mu.Unlock()
	if status == "done" {
		db.Exec("UPDATE files SET status=?, output_size=?, error='', updated_at=? WHERE id=?", status, outputSize, time.Now(), fileID)
	} else {
		db.Exec("UPDATE files SET status=?, output_size=?, error=?, updated_at=? WHERE id=?", status, outputSize, errStr, time.Now(), fileID)
	}
}
