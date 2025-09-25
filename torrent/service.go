package torrent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/felipemarinho97/magnet-metadata-api/config"
	"github.com/felipemarinho97/magnet-metadata-api/model"
	"github.com/felipemarinho97/magnet-metadata-api/util"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/mux"
)

type TorrentService struct {
	config      *config.Config
	client      *torrent.Client
	redisClient *redis.Client
	ctx         context.Context
	fileCount   int64

	// Lock mechanism for preventing concurrent requests for same hash
	hashLocks  map[string]*sync.Mutex
	lockMapMux sync.RWMutex

	// Dead Letter Queue for failed torrents
	dlqFile string
	dlqMux  sync.RWMutex

	// Activity tracking for smart DLQ processing
	activeITorrentsRequests int32 // atomic counter
	dlqPaused               chan bool
	dlqResume               chan bool
}

func NewTorrentService(config *config.Config) (*TorrentService, error) {
	ctx := context.Background()

	// Setup Redis client
	opt, err := redis.ParseURL(config.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("invalid redis URL: %w", err)
	}
	redisClient := redis.NewClient(opt)

	// Test Redis connection
	if err := redisClient.Ping(ctx).Err(); err != nil {
		log.Printf("Warning: Redis connection failed: %v. Using disk cache only.", err)
		redisClient = nil
	}

	// Create cache directory
	if err := os.MkdirAll(config.CacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Configure torrent client
	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.DataDir = config.CacheDir
	clientConfig.ListenPort = config.ClientPort
	clientConfig.DisableTrackers = false
	clientConfig.NoDHT = false
	clientConfig.DisableUTP = false
	clientConfig.Seed = config.SeedingEnabled
	clientConfig.DhtStartingNodes = func(network string) dht.StartingNodesGetter {
		return func() ([]dht.Addr, error) {
			nodes := make([]dht.Addr, len(config.DHTPeers))
			for i, peer := range config.DHTPeers {
				port, err := strconv.Atoi(strings.Split(peer, ":")[1])
				if err != nil {
					return nil, fmt.Errorf("invalid DHT peer port: %w", err)
				}
				nodes[i] = dht.NewAddr(&net.TCPAddr{
					IP:   net.ParseIP(strings.Split(peer, ":")[0]),
					Port: port,
				})
			}
			return nodes, nil
		}
	}

	// Additional settings to prevent downloading
	clientConfig.DisableAggressiveUpload = true
	clientConfig.DisableAcceptRateLimiting = true

	client, err := torrent.NewClient(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create torrent client: %w", err)
	}

	fileCount, err := util.CountDir(config.CacheDir)
	if err != nil {
		fmt.Printf("Warning: Failed to count cached files: %v. Using 0 as initial count.", err)
		fileCount = 0
	}

	// update fileCount every 10 minutes
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			count, err := util.CountDir(config.CacheDir)
			if err != nil {
				log.Printf("Warning: Failed to update cached file count: %v", err)
			} else {
				fileCount = count
				log.Printf("Updated cached file count: %d", fileCount)
			}
		}
	}()

	initialChunkSizeStr := os.Getenv("FALLBACK_INITIAL_CHUNK_SIZE_KB")
	if initialChunkSizeStr != "" {
		if size, err := strconv.Atoi(initialChunkSizeStr); err == nil && size > 0 {
			initialChunkSize = size * 1024 // Convert to bytes
		} else {
			log.Printf("Invalid FALLBACK_INITIAL_CHUNK_SIZE_KB value: %d, using default", initialChunkSize/1024)
		}
	}

	service := &TorrentService{
		config:                  config,
		client:                  client,
		redisClient:             redisClient,
		ctx:                     ctx,
		fileCount:               fileCount,
		hashLocks:               make(map[string]*sync.Mutex),
		lockMapMux:              sync.RWMutex{},
		dlqFile:                 filepath.Join(config.CacheDir, "dlq.json"),
		activeITorrentsRequests: 0,
		dlqPaused:               make(chan bool, 1),
		dlqResume:               make(chan bool, 1),
	}

	// Start background DLQ processor
	go service.processDLQ()

	return service, nil
}

func (ts *TorrentService) Close() error {
	if ts.client != nil {
		ts.client.Close()
	}
	if ts.redisClient != nil {
		return ts.redisClient.Close()
	}
	return nil
}

// getOrCreateHashLock returns a lock for the given hash, creating one if it doesn't exist
func (ts *TorrentService) getOrCreateHashLock(infoHash string) *sync.Mutex {
	ts.lockMapMux.RLock()
	if lock, exists := ts.hashLocks[infoHash]; exists {
		ts.lockMapMux.RUnlock()
		return lock
	}
	ts.lockMapMux.RUnlock()

	// Need to create a new lock
	ts.lockMapMux.Lock()
	defer ts.lockMapMux.Unlock()

	// Double-check in case another goroutine created it while we were waiting
	if lock, exists := ts.hashLocks[infoHash]; exists {
		return lock
	}

	// Create new lock
	lock := &sync.Mutex{}
	ts.hashLocks[infoHash] = lock
	return lock
}

// cleanupHashLock removes the lock for a hash if it's no longer needed
func (ts *TorrentService) cleanupHashLock(infoHash string) {
	ts.lockMapMux.Lock()
	defer ts.lockMapMux.Unlock()

	// Only delete if no one is waiting on it
	if lock, exists := ts.hashLocks[infoHash]; exists {
		// Try to acquire the lock immediately to see if anyone is waiting
		if lock.TryLock() {
			delete(ts.hashLocks, infoHash)
			lock.Unlock()
		}
	}
}

func (ts *TorrentService) parseMagnetURI(magnetURI string) (metainfo.Hash, error) {
	magnet, err := metainfo.ParseMagnetUri(magnetURI)
	if err != nil {
		return metainfo.Hash{}, fmt.Errorf("failed to parse magnet URI: %w", err)
	}

	infoHash := magnet.InfoHash

	return infoHash, nil
}

func (ts *TorrentService) getCachedMetadata(infoHash string) (*model.TorrentMetadata, error) {
	// Try Redis cache first
	if ts.redisClient != nil {
		key := strings.ToLower("metadata:" + infoHash)
		cached, err := ts.redisClient.Get(ts.ctx, key).Result()
		if err == nil {
			var metadata model.TorrentMetadata
			if err := json.Unmarshal([]byte(cached), &metadata); err == nil {
				return &metadata, nil
			}
		}
	}

	// Try disk cache
	cachePath := filepath.Join(ts.config.CacheDir, infoHash+".json")
	if data, err := os.ReadFile(cachePath); err == nil {
		var metadata model.TorrentMetadata
		if err := json.Unmarshal(data, &metadata); err == nil {
			return &metadata, nil
		}
	}

	return nil, nil
}

func (ts *TorrentService) cacheMetadata(metadata *model.TorrentMetadata) error {
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}

	// Cache in Redis with 24h expiration
	if ts.redisClient != nil {
		key := strings.ToLower("metadata:" + metadata.InfoHash)
		ts.redisClient.Set(ts.ctx, key, data, 24*time.Hour)
	}

	// Cache on disk
	cachePath := filepath.Join(ts.config.CacheDir, metadata.InfoHash+".json")
	return os.WriteFile(cachePath, data, 0644)
}

func (ts *TorrentService) saveTorrentFile(t *torrent.Torrent, infoHashStr string) error {
	// Save the .torrent file to cache
	torrentPath := filepath.Join(ts.config.CacheDir, infoHashStr+".torrent")

	// Get the metainfo and write to file
	metainfo := t.Metainfo()
	var buf bytes.Buffer
	err := metainfo.Write(&buf)
	torrentData := buf.Bytes()
	if err != nil {
		return fmt.Errorf("failed to marshal torrent file: %w", err)
	}

	return os.WriteFile(torrentPath, torrentData, 0644)
}

func (ts *TorrentService) getTorrentMetadata(ctx context.Context, magnetURI string) (*model.TorrentMetadata, error) {
	infoHash, err := ts.parseMagnetURI(magnetURI)
	if err != nil {
		return nil, err
	}

	infoHashStr := infoHash.String()

	log.Printf("Fetching metadata for info hash: %s", infoHashStr)

	// Check if torrent is already added to client
	existingTorrent, exists := ts.client.Torrent(infoHash)
	if exists && existingTorrent.Info() != nil {
		log.Printf("Torrent already exists in client with info: %s", infoHashStr)
		return ts.extractMetadataFromTorrent(existingTorrent, infoHashStr)
	}

	// Add torrent to client
	t, err := ts.client.AddMagnet(magnetURI)
	if err != nil {
		return nil, fmt.Errorf("failed to add magnet: %w", err)
	}

	// Ensure we clean up the torrent when done
	defer func() {
		if t != nil {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Torrent already dropped: %s", infoHashStr)
				}
			}()
			t.Drop()
		}
	}()

	t.DisallowDataDownload()

	// Wait for info with timeout
	select {
	case <-t.GotInfo():
		log.Printf("Got info for torrent: %s", t.Name())
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("timeout waiting for torrent info")
	case <-ctx.Done():
		return nil, fmt.Errorf("context cancelled while waiting for torrent info")
	}

	return ts.extractMetadataFromTorrent(t, infoHashStr)
}

// extractMetadataFromTorrent extracts metadata from a torrent object
func (ts *TorrentService) extractMetadataFromTorrent(t *torrent.Torrent, infoHashStr string) (*model.TorrentMetadata, error) {
	// Extract metadata
	info := t.Info()
	if info == nil {
		return nil, fmt.Errorf("failed to get torrent info")
	}

	files := make([]model.FileInfo, len(info.Files))
	var offset int64
	for i, file := range info.Files {
		files[i] = model.FileInfo{
			Path:   strings.Join(file.Path, "/"),
			Size:   file.Length,
			Offset: offset,
		}
		offset += file.Length
	}
	// Sort files by size (descending)
	sort.Slice(files, func(i, j int) bool {
		return files[i].Size > files[j].Size
	})

	metadata := &model.TorrentMetadata{
		InfoHash: infoHashStr,
		Name:     info.Name,
		Size:     info.TotalLength(),
		Files:    files,
		Comment:  t.Metainfo().Comment,
	}

	if !(t.Metainfo().CreationDate == 0) {
		timeParsed := time.Unix(t.Metainfo().CreationDate, 0)
		metadata.CreatedAt = &timeParsed
	}

	// Extract trackers
	for _, tier := range t.Metainfo().AnnounceList {
		metadata.Trackers = append(metadata.Trackers, tier...)
	}

	// Add download URL if enabled
	if ts.config.EnableDownloads {
		downloadURL := fmt.Sprintf("/download/%s", infoHashStr)
		metadata.DownloadURL = &downloadURL
	} else {
		metadata.DownloadURL = nil
	}

	// Save the .torrent file before returning
	if err := ts.saveTorrentFile(t, infoHashStr); err != nil {
		log.Printf("Failed to save torrent file: %v", err)
	}

	// Cache the metadata
	if err := ts.cacheMetadata(metadata); err != nil {
		log.Printf("Failed to cache metadata: %v", err)
	}

	return metadata, nil
}

func (ts *TorrentService) handleGetMetadata(w http.ResponseWriter, r *http.Request) {
	var req model.MagnetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ts.writeError(w, http.StatusBadRequest, "Invalid JSON", err.Error())
		return
	}

	if req.MagnetURI == "" {
		ts.writeError(w, http.StatusBadRequest, "Missing magnet URI", "magnet_uri field is required")
		return
	}

	// Parse magnet URI to get info hash early
	infoHash, err := ts.parseMagnetURI(req.MagnetURI)
	if err != nil {
		ts.writeError(w, http.StatusBadRequest, "Invalid magnet URI", err.Error())
		return
	}

	infoHashStr := infoHash.String()

	// Get or create a lock for this specific hash to prevent duplicate scraping
	hashLock := ts.getOrCreateHashLock(infoHashStr)

	// Acquire the lock for this hash
	hashLock.Lock()
	defer func() {
		hashLock.Unlock()
		// Clean up the lock if possible (non-blocking)
		go ts.cleanupHashLock(infoHashStr)
	}()

	log.Printf("Acquired lock for request processing: %s", infoHashStr)

	// Check cache after acquiring lock (another request might have filled it)
	if cached, err := ts.getCachedMetadata(infoHashStr); cached != nil && err == nil {
		log.Printf("Cache hit after lock acquisition for info hash: %s", infoHashStr)
		w.Header().Set("Content-Type", "application/json")
		err = json.NewEncoder(w).Encode(cached)
		if err != nil {
			log.Printf("Error encoding cached metadata: %v", err)
			ts.writeError(w, http.StatusInternalServerError, "Encoding error", "Failed to encode cached metadata")
		}
		return
	}

	log.Printf("Cache miss after lock acquisition, proceeding with scraping: %s", infoHashStr)

	type result struct {
		metadata any
		err      error
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	resultCh := make(chan result, 2)

	go func() {
		metadata, err := ts.getTorrentMetadata(ctx, req.MagnetURI)
		select {
		case resultCh <- result{metadata, err}:
		case <-ctx.Done():
		}
	}()

	go func() {
		metadata, err := ts.getMetadataFromITorrentsWithTracking(infoHashStr)
		select {
		case resultCh <- result{metadata, err}:
		case <-ctx.Done():
		}
	}()

	var lastErr error
	for i := 0; i < 2; i++ {
		select {
		case res := <-resultCh:
			if res.err == nil {
				// Success! Remove from DLQ if it was there
				ts.removeTorrentFromDLQ(infoHashStr)
				w.Header().Set("Content-Type", "application/json")
				err = json.NewEncoder(w).Encode(res.metadata)
				if err != nil {
					log.Printf("Error encoding metadata: %v", err)
					ts.writeError(w, http.StatusInternalServerError, "Encoding error", "Failed to encode metadata")
				}
				return
			}
			log.Printf("Error retrieving metadata: %v", res.err)
			lastErr = res.err
		case <-ctx.Done():
			// Add to DLQ on timeout
			ts.addToDLQ(infoHashStr, req.MagnetURI, "Timeout while retrieving metadata")
			ts.writeError(w, http.StatusGatewayTimeout, "Timeout", "Timeout while retrieving metadata")
			return
		}
	}

	// Both services failed, add to DLQ for later retry
	ts.addToDLQ(infoHashStr, req.MagnetURI, lastErr.Error())
	ts.writeError(w, http.StatusInternalServerError, "Failed to get torrent metadata", lastErr.Error())
}

func (ts *TorrentService) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !ts.config.EnableDownloads {
		ts.writeError(w, http.StatusForbidden, "Downloads disabled", "Download functionality is disabled")
		return
	}

	vars := mux.Vars(r)
	infoHash := vars["hash"]

	if len(infoHash) != 40 {
		ts.writeError(w, http.StatusBadRequest, "Invalid info hash", "Info hash must be 40 characters")
		return
	}

	// Check if we have the torrent file cached
	torrentPath := filepath.Join(ts.config.CacheDir, infoHash+".torrent")

	// Try to serve from cache first
	if data, err := os.ReadFile(torrentPath); err == nil {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.torrent\"", infoHash))
		_, err := w.Write(data)
		if err != nil {
			log.Printf("Error writing torrent file: %v", err)
			ts.writeError(w, http.StatusInternalServerError, "Write error", "Failed to write torrent file")
		}
		return
	}

	ts.writeError(w, http.StatusNotFound, "Torrent file not found", "Torrent file not available in cache")
}

func (ts *TorrentService) handleHealth(w http.ResponseWriter, r *http.Request) {
	ts.lockMapMux.RLock()
	activeLocks := len(ts.hashLocks)
	ts.lockMapMux.RUnlock()

	ts.dlqMux.RLock()
	dlqEntries := ts.loadDLQ()
	ts.dlqMux.RUnlock()

	health := map[string]interface{}{
		"status": "ok",
		"stats": map[string]interface{}{
			"active_torrents":           len(ts.client.Torrents()) + int(ts.fileCount),
			"active_locks":              activeLocks,
			"dlq_entries":               len(dlqEntries),
			"active_itorrents_requests": ts.activeITorrentsRequests,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(health)
	if err != nil {
		log.Printf("Error encoding health response: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
}

func (ts *TorrentService) handleGetDLQ(w http.ResponseWriter, r *http.Request) {
	ts.dlqMux.RLock()
	entries := ts.loadDLQ()
	ts.dlqMux.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(map[string]interface{}{
		"entries": entries,
		"count":   len(entries),
	})
	if err != nil {
		log.Printf("Error encoding DLQ response: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
}

func (ts *TorrentService) handleForceDLQ(w http.ResponseWriter, r *http.Request) {
	// Create a context that cancels when the request is cancelled
	ctx := r.Context()

	// Set up streaming response
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Start streaming response
	enc := json.NewEncoder(w)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	// Send initial response
	enc.Encode(map[string]interface{}{
		"message": "Force DLQ processing started",
		"status":  "processing",
	})
	flusher.Flush()

	processed := ts.forceDLQProcessWithContext(ctx, func(update map[string]interface{}) {
		enc.Encode(update)
		flusher.Flush()
	})

	// Send final response
	enc.Encode(map[string]interface{}{
		"message":   "Force DLQ processing completed",
		"processed": processed,
		"status":    "completed",
	})
}

func (ts *TorrentService) writeError(w http.ResponseWriter, status int, error, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	err := json.NewEncoder(w).Encode(model.ErrorResponse{
		Error:   error,
		Message: message,
	})
	if err != nil {
		log.Printf("Error encoding error response: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
}

func (ts *TorrentService) SetupRoutes() *mux.Router {
	r := mux.NewRouter()

	// API routes
	api := r.PathPrefix("/api/v1").Subrouter()
	api.HandleFunc("/metadata", ts.handleGetMetadata).Methods("POST")
	api.HandleFunc("/health", ts.handleHealth).Methods("GET")
	api.HandleFunc("/dlq", ts.handleGetDLQ).Methods("GET")
	api.HandleFunc("/dlq/process", ts.handleForceDLQ).Methods("POST")

	// Download route (if enabled)
	if ts.config.EnableDownloads {
		r.HandleFunc("/download/{hash}", ts.handleDownload).Methods("GET")
	}

	// Serve static files from /web at root path
	fs := http.FileServer(http.Dir("web"))
	r.PathPrefix("/").Handler(fs)

	// Middleware
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}

			log.Printf("%s %s", r.Method, r.URL.Path)
			next.ServeHTTP(w, r)
		})
	})

	return r
}

// getMetadataFromITorrentsWithTracking wraps the fallback method with activity tracking
func (ts *TorrentService) getMetadataFromITorrentsWithTracking(infoHash string) (*model.TorrentMetadata, error) {
	// Increment counter and signal DLQ to pause
	atomic.AddInt32(&ts.activeITorrentsRequests, 1)
	select {
	case ts.dlqPaused <- true:
	default: // Channel might be full, that's ok
	}

	defer func() {
		// Decrement counter and signal DLQ to resume if no more active requests
		if atomic.AddInt32(&ts.activeITorrentsRequests, -1) == 0 {
			select {
			case ts.dlqResume <- true:
			default: // Channel might be full, that's ok
			}
		}
	}()

	return ts.getMetadataFromITorrents(infoHash)
}

// addToDLQ adds a failed torrent to the dead letter queue
func (ts *TorrentService) addToDLQ(infoHash, magnetURI, errorMsg string) {
	ts.dlqMux.Lock()
	defer ts.dlqMux.Unlock()

	entries := ts.loadDLQ()
	now := time.Now()

	// Check if entry already exists
	for i, entry := range entries {
		if entry.InfoHash == infoHash {
			entries[i].FailCount++
			entries[i].LastAttempt = now
			entries[i].LastError = errorMsg
			ts.saveDLQ(entries)
			log.Printf("Updated DLQ entry for %s, fail count: %d", infoHash, entries[i].FailCount)
			return
		}
	}

	// Add new entry
	newEntry := model.DLQEntry{
		InfoHash:     infoHash,
		MagnetURI:    magnetURI,
		FailCount:    1,
		LastAttempt:  now,
		FirstFailure: now,
		LastError:    errorMsg,
	}
	entries = append(entries, newEntry)
	ts.saveDLQ(entries)
	log.Printf("Added new DLQ entry for %s", infoHash)
}

// loadDLQ loads DLQ entries from disk
func (ts *TorrentService) loadDLQ() []model.DLQEntry {
	data, err := os.ReadFile(ts.dlqFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Error reading DLQ file: %v", err)
		}
		return []model.DLQEntry{}
	}

	var entries []model.DLQEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Printf("Error unmarshaling DLQ: %v", err)
		return []model.DLQEntry{}
	}

	return entries
}

// saveDLQ saves DLQ entries to disk
func (ts *TorrentService) saveDLQ(entries []model.DLQEntry) {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		log.Printf("Error marshaling DLQ: %v", err)
		return
	}

	if err := os.WriteFile(ts.dlqFile, data, 0644); err != nil {
		log.Printf("Error writing DLQ file: %v", err)
	}
}

// removeTorrentFromDLQ removes a torrent from DLQ after successful processing
func (ts *TorrentService) removeTorrentFromDLQ(infoHash string) {
	ts.dlqMux.Lock()
	defer ts.dlqMux.Unlock()

	entries := ts.loadDLQ()
	for i, entry := range entries {
		if entry.InfoHash == infoHash {
			// Remove entry by swapping with last element and truncating
			entries[i] = entries[len(entries)-1]
			entries = entries[:len(entries)-1]
			ts.saveDLQ(entries)
			log.Printf("Removed %s from DLQ after successful processing", infoHash)
			return
		}
	}
}

// processDLQ sequential background service to retry failed torrents
func (ts *TorrentService) processDLQ() {
	log.Printf("DLQ processor started")
	isPaused := false

	for {
		select {
		case <-ts.dlqPaused:
			if !isPaused {
				log.Printf("DLQ processor paused (iTorrents activity detected)")
				isPaused = true
			}

		case <-ts.dlqResume:
			if isPaused {
				log.Printf("DLQ processor resumed (iTorrents activity stopped)")
				isPaused = false
			}

		case <-ts.ctx.Done():
			log.Printf("DLQ processor stopped")
			return

		default:
			if !isPaused {
				// Process one item
				ts.processNextDLQEntry()

				// Wait 1 second before next item
				select {
				case <-time.After(1 * time.Second):
				case <-ts.ctx.Done():
					return
				}
			} else {
				// When paused, check for resume events more frequently
				time.Sleep(1000 * time.Millisecond)
			}
		}
	}
}

// processNextDLQEntry processes one entry from the DLQ, returns true if processed something
func (ts *TorrentService) processNextDLQEntry() bool {
	ts.dlqMux.RLock()
	entries := ts.loadDLQ()
	ts.dlqMux.RUnlock()

	if len(entries) == 0 {
		return false
	}

	// Find entry that's eligible for retry (not attempted recently and hasn't failed too many times)
	now := time.Now()
	var selectedEntry *model.DLQEntry

	for i, entry := range entries {
		// Skip if attempted recently (wait longer based on fail count)
		backoffMinutes := math.Min(float64(entry.FailCount*30), 240) // Max 4 hours backoff
		if now.Sub(entry.LastAttempt) < time.Duration(backoffMinutes)*time.Minute {
			continue
		}

		// Skip if failed too many times
		if entry.FailCount >= 10 {
			continue
		}

		selectedEntry = &entries[i]
		break
	}

	if selectedEntry == nil {
		return false
	}

	log.Printf("Retrying DLQ entry: %s (attempt %d)", selectedEntry.InfoHash, selectedEntry.FailCount+1)

	// Try to get metadata using fallback only
	metadata, err := ts.getMetadataFromITorrents(selectedEntry.InfoHash)
	if err != nil {
		// Update failure
		ts.addToDLQ(selectedEntry.InfoHash, selectedEntry.MagnetURI, err.Error())
		log.Printf("DLQ retry failed for %s: %v", selectedEntry.InfoHash, err)
		return true
	}

	// Success! Cache the metadata and remove from DLQ
	if err := ts.cacheMetadata(metadata); err != nil {
		log.Printf("Failed to cache DLQ success for %s: %v", selectedEntry.InfoHash, err)
	}

	ts.removeTorrentFromDLQ(selectedEntry.InfoHash)
	log.Printf("DLQ retry succeeded for %s", selectedEntry.InfoHash)
	return true
}

// forceDLQProcess processes multiple DLQ entries ignoring LastAttempt timing (legacy method)
func (ts *TorrentService) forceDLQProcess() int {
	return ts.forceDLQProcessWithContext(context.Background(), nil)
}

// forceDLQProcessWithContext processes DLQ entries with context cancellation and progress updates
func (ts *TorrentService) forceDLQProcessWithContext(ctx context.Context, progressCallback func(map[string]interface{})) int {
	ts.dlqMux.RLock()
	entries := ts.loadDLQ()
	ts.dlqMux.RUnlock()

	if len(entries) == 0 {
		if progressCallback != nil {
			progressCallback(map[string]interface{}{
				"message":   "No entries in DLQ",
				"processed": 0,
			})
		}
		return 0
	}

	processed := 0
	processingDelay := 3 * time.Second // Slower processing for force mode

	log.Printf("Force processing DLQ entries, found %d entries", len(entries))

	if progressCallback != nil {
		progressCallback(map[string]interface{}{
			"message":       fmt.Sprintf("Found %d entries to process", len(entries)),
			"total_entries": len(entries),
		})
	}

	for i, entry := range entries {
		select {
		case <-ctx.Done():
			log.Printf("Force DLQ processing cancelled after %d entries", processed)
			if progressCallback != nil {
				progressCallback(map[string]interface{}{
					"message":   "Processing cancelled by client",
					"processed": processed,
					"cancelled": true,
				})
			}
			return processed
		default:
		}

		// Skip if failed too many times
		if entry.FailCount >= 15 {
			continue
		}

		log.Printf("Force retrying DLQ entry: %s (attempt %d)", entry.InfoHash, entry.FailCount+1)

		if progressCallback != nil {
			progressCallback(map[string]interface{}{
				"message":      fmt.Sprintf("Processing entry %d/%d: %s", i+1, len(entries), entry.InfoHash),
				"current_hash": entry.InfoHash,
				"attempt":      entry.FailCount + 1,
			})
		}

		// Try to get metadata using fallback only
		metadata, err := ts.getMetadataFromITorrents(entry.InfoHash)
		if err != nil {
			// Update failure
			ts.addToDLQ(entry.InfoHash, entry.MagnetURI, err.Error())
			log.Printf("Force DLQ retry failed for %s: %v", entry.InfoHash, err)
			if progressCallback != nil {
				progressCallback(map[string]interface{}{
					"message": fmt.Sprintf("Failed to process %s: %v", entry.InfoHash, err),
					"hash":    entry.InfoHash,
					"status":  "failed",
				})
			}
			continue
		}

		// Success! Cache the metadata and remove from DLQ
		if err := ts.cacheMetadata(metadata); err != nil {
			log.Printf("Failed to cache force DLQ success for %s: %v", entry.InfoHash, err)
		}

		ts.removeTorrentFromDLQ(entry.InfoHash)
		log.Printf("Force DLQ retry succeeded for %s", entry.InfoHash)
		processed++

		if progressCallback != nil {
			progressCallback(map[string]interface{}{
				"message":   fmt.Sprintf("Successfully processed %s", entry.InfoHash),
				"hash":      entry.InfoHash,
				"status":    "success",
				"processed": processed,
			})
		}

		// Add delay between processing (unless cancelled)
		select {
		case <-ctx.Done():
			return processed
		case <-time.After(processingDelay):
		}
	}

	log.Printf("Force DLQ processing completed, processed %d entries", processed)
	return processed
}
