package torrent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/felipemarinho97/magnet-metadata-api/model"
)

// BitmagnetGraphQLRequest represents the GraphQL request structure
type BitmagnetGraphQLRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

// BitmagnetResponse represents the response from Bitmagnet GraphQL API
type BitmagnetResponse struct {
	Data   BitmagnetData  `json:"data"`
	Errors []GraphQLError `json:"errors,omitempty"`
}

type GraphQLError struct {
	Message string   `json:"message"`
	Path    []string `json:"path,omitempty"`
}

type BitmagnetData struct {
	TorrentInfo  TorrentContentSearch `json:"torrentInfo"`
	TorrentFiles TorrentFilesResult   `json:"torrentFiles"`
}

type TorrentContentSearch struct {
	Search TorrentSearchResult `json:"search"`
}

type TorrentSearchResult struct {
	Items []TorrentSearchItem `json:"items"`
}

type TorrentSearchItem struct {
	InfoHash string           `json:"infoHash"`
	Title    string           `json:"title"`
	Torrent  BitmagnetTorrent `json:"torrent"`
}

type BitmagnetTorrent struct {
	InfoHash   string    `json:"infoHash"`
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	FilesCount int       `json:"filesCount"`
	Seeders    *int      `json:"seeders"`
	Leechers   *int      `json:"leechers"`
	MagnetUri  string    `json:"magnetUri"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type TorrentFilesResult struct {
	Files TorrentFiles `json:"files"`
}

type TorrentFiles struct {
	TotalCount int                 `json:"totalCount"`
	Items      []BitmagnetFileInfo `json:"items"`
}

type BitmagnetFileInfo struct {
	Index     int    `json:"index"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Extension string `json:"extension"`
	FileType  string `json:"fileType"`
}

var (
	bitmagnetClient = &http.Client{
		Timeout: 30 * time.Second,
	}

	// Rate limiter for Bitmagnet requests
	bitmagnetRateLimiter = NewRateLimiter(5.0, 10) // 5 req/s with burst of 10
)

// getMetadataFromBitmagnet fetches torrent metadata from Bitmagnet GraphQL API
func (ts *TorrentService) getMetadataFromBitmagnet(infoHash string) (*model.TorrentMetadata, error) {
	bitmagnetURL := os.Getenv("BITMAGNET_URL")
	if bitmagnetURL == "" {
		return nil, fmt.Errorf("[bitmagnet] BITMAGNET_URL environment variable not set")
	}

	// Ensure URL ends with /graphql
	if !strings.HasSuffix(bitmagnetURL, "/graphql") {
		if !strings.HasSuffix(bitmagnetURL, "/") {
			bitmagnetURL += "/"
		}
		bitmagnetURL += "graphql"
	}

	// Rate limit the request
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := bitmagnetRateLimiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("[bitmagnet] rate limiter timeout: %w", err)
	}

	// Convert info hash to uppercase for consistency
	infoHashUpper := strings.ToUpper(infoHash)

	// Prepare GraphQL query and variables
	query := `
		query GetTorrentMetadata($infoHashString: String!, $infoHashHash: Hash20!) {
			torrentInfo: torrentContent {
				search(input: { 
					queryString: $infoHashString, 
					limit: 1 
				}) {
					items {
						infoHash
						title
						torrent {
							infoHash
							name
							size
							filesCount
							seeders
							leechers
							magnetUri
							createdAt
							updatedAt
						}
					}
				}
			}
			
			torrentFiles: torrent {
				files(input: { 
					infoHashes: [$infoHashHash],
					limit: 100
				}) {
					totalCount
					items {
						index
						path
						size
						extension
						fileType
					}
				}
			}
		}`

	variables := map[string]interface{}{
		"infoHashString": infoHashUpper,
		"infoHashHash":   infoHashUpper,
	}

	request := BitmagnetGraphQLRequest{
		Query:     query,
		Variables: variables,
	}

	// Marshal request to JSON
	requestBody, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("[bitmagnet] failed to marshal request: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", bitmagnetURL, bytes.NewBuffer(requestBody))
	if err != nil {
		return nil, fmt.Errorf("[bitmagnet] failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// Execute request
	resp, err := bitmagnetClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("[bitmagnet] request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[bitmagnet] HTTP error: %d %s", resp.StatusCode, resp.Status)
	}

	// Parse response
	var bitmagnetResp BitmagnetResponse
	if err := json.NewDecoder(resp.Body).Decode(&bitmagnetResp); err != nil {
		return nil, fmt.Errorf("[bitmagnet] failed to decode response: %w", err)
	}

	// Check for GraphQL errors
	if len(bitmagnetResp.Errors) > 0 {
		return nil, fmt.Errorf("[bitmagnet] GraphQL errors: %v", bitmagnetResp.Errors)
	}

	// Check if we found any results
	if len(bitmagnetResp.Data.TorrentInfo.Search.Items) == 0 {
		return nil, fmt.Errorf("[bitmagnet] no torrent found for hash: %s", infoHash)
	}

	// Extract torrent info
	torrentItem := bitmagnetResp.Data.TorrentInfo.Search.Items[0]
	torrentInfo := torrentItem.Torrent

	// Build metadata
	metadata := &model.TorrentMetadata{
		InfoHash: infoHashUpper, // Use the normalized uppercase version
		Name:     torrentInfo.Name,
		Size:     torrentInfo.Size,
	}

	// Set creation date
	if !torrentInfo.CreatedAt.IsZero() {
		metadata.CreatedAt = &torrentInfo.CreatedAt
	}

	// Extract trackers from magnet URI if available
	if torrentInfo.MagnetUri != "" {
		trackers := extractTrackersFromMagnet(torrentInfo.MagnetUri)
		metadata.Trackers = trackers
	}

	// Process files if available
	if len(bitmagnetResp.Data.TorrentFiles.Files.Items) > 0 {
		files := make([]model.FileInfo, 0, len(bitmagnetResp.Data.TorrentFiles.Files.Items))
		var offset int64

		for _, file := range bitmagnetResp.Data.TorrentFiles.Files.Items {
			files = append(files, model.FileInfo{
				Path:   file.Path,
				Size:   file.Size,
				Offset: offset,
			})
			offset += file.Size
		}

		metadata.Files = files
	} else {
		// Single file torrent or no file info available
		metadata.Files = []model.FileInfo{
			{
				Path:   metadata.Name,
				Size:   metadata.Size,
				Offset: 0,
			},
		}
	}

	// Cache the metadata
	if err := ts.cacheMetadata(metadata); err != nil {
		log.Printf("[bitmagnet] warning: failed to cache metadata: %v", err)
	}

	// ensure returned info hash matches requested (case-insensitive)
	if strings.ToLower(infoHash) != strings.ToLower(metadata.InfoHash) {
		return nil, fmt.Errorf("[bitmagnet] info hash mismatch: requested %s but got %s", infoHashUpper, metadata.InfoHash)
	}

	log.Printf("[bitmagnet] Successfully retrieved metadata for torrent hash: %s", infoHashUpper)

	return metadata, nil
}

// extractTrackersFromMagnet extracts tracker URLs from a magnet URI
func extractTrackersFromMagnet(magnetURI string) []string {
	var trackers []string

	// Simple parsing to extract tr= parameters
	parts := strings.Split(magnetURI, "&")
	for _, part := range parts {
		if strings.HasPrefix(part, "tr=") {
			tracker := strings.TrimPrefix(part, "tr=")
			// URL decode the tracker
			if decodedTracker, err := parseURLEncoded(tracker); err == nil {
				trackers = append(trackers, decodedTracker)
			} else {
				trackers = append(trackers, tracker)
			}
		}
	}

	return trackers
}

// parseURLEncoded is a simple URL decoder for tracker URLs
func parseURLEncoded(s string) (string, error) {
	// Replace %XX with actual characters
	result := strings.ReplaceAll(s, "%3A", ":")
	result = strings.ReplaceAll(result, "%2F", "/")
	result = strings.ReplaceAll(result, "%3F", "?")
	result = strings.ReplaceAll(result, "%3D", "=")
	result = strings.ReplaceAll(result, "%26", "&")
	result = strings.ReplaceAll(result, "%20", " ")

	return result, nil
}

// UpdateBitmagnetRateLimit allows dynamic adjustment of the rate limit
func UpdateBitmagnetRateLimit(requestsPerSecond float64, burstSize int) {
	bitmagnetRateLimiter.UpdateLimit(requestsPerSecond, burstSize)
	log.Printf("[rate-limiter] Updated Bitmagnet rate limit to %.2f req/s with burst %d", requestsPerSecond, burstSize)
}
