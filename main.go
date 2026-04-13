package main

import (
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/felipemarinho97/magnet-metadata-api/config"
	"github.com/felipemarinho97/magnet-metadata-api/torrent"
)

func AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		if strings.HasPrefix(r.URL.Path, "/api/v1/health") {
			next.ServeHTTP(w, r)
			return
		}

		token := r.Header.Get("Authorization")
		expected := "Bearer " + os.Getenv("API_KEY")

		if token != expected {
			log.Printf("Unauthorized request: %s %s from %s",
				r.Method, r.URL.Path, r.RemoteAddr)

			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func main() {
	// Add global panic recovery
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Application panic recovered: %v", r)
			log.Fatalf("Application crashed due to panic: %v", r)
		}
	}()

	config := config.LoadConfig()

	log.Printf("Starting Torrent Metadata API Service")
	log.Printf("Config: Port=%s, CacheDir=%s, EnableDownloads=%v, SeedingEnabled=%v",
		config.Port, config.CacheDir, config.EnableDownloads, config.SeedingEnabled)

	service, err := torrent.NewTorrentService(config)
	if err != nil {
		log.Fatalf("Failed to create service: %v", err)
	}
	defer service.Close()

	router := service.SetupRoutes()

	protectedRouter := AuthMiddleware(router)

	log.Printf("Server listening on port %s", config.Port)
	if err := http.ListenAndServe(":"+config.Port, protectedRouter); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
