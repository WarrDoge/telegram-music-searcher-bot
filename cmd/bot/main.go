// Main entry point for the Telegram Music Searcher Bot
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"telegram-music-bot/internal/bot"
	"telegram-music-bot/internal/config"
)

func main() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("❌ Configuration error: %v", err)
	}

	// Create bot instance
	musicBot, err := bot.New(cfg)
	if err != nil {
		log.Fatalf("❌ Failed to create bot: %v", err)
	}

	// Start health check server
	go startHealthCheckServer()

	// Setup graceful shutdown
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Run the bot
	if err := musicBot.Run(ctx); err != nil {
		log.Fatalf("❌ Bot error: %v", err)
	}

	log.Println("✅ Bot shut down successfully")
}

// startHealthCheckServer starts an HTTP server for health checks
func startHealthCheckServer() {
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	http.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("READY"))
	})

	port := os.Getenv("HEALTH_PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("🏥 Health check server listening on :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Printf("⚠️  Health check server error: %v", err)
	}
}
