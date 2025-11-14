// Package bot implements the main music bot logic
package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"go.uber.org/zap"

	"telegram-music-bot/internal/cache"
	"telegram-music-bot/internal/config"
	httpclient "telegram-music-bot/internal/http"
	"telegram-music-bot/internal/platform"
	"telegram-music-bot/internal/search"
	"telegram-music-bot/internal/telegram"
)

// MusicBot is the main bot instance
type MusicBot struct {
	config      *config.Config
	telegram    *telegram.Client
	httpClient  *httpclient.Client
	logger      *zap.Logger
	orchestrator *search.Orchestrator

	// Caches
	queryCache    *cache.SimpleCache
	urlCache      *cache.SimpleCache
	negativeCache *cache.SimpleCache

	// Circuit Breakers
	spotifyBreaker    *httpclient.CircuitBreaker
	youtubeBreaker    *httpclient.CircuitBreaker
	appleMusicBreaker *httpclient.CircuitBreaker

	// Semaphores for concurrency control
	fetchSem *httpclient.Semaphore
	msgSem   *httpclient.Semaphore
}

// New creates a new MusicBot instance
func New(cfg *config.Config) (*MusicBot, error) {
	// Initialize logger
	logger, err := createLogger(cfg.Debug)
	if err != nil {
		return nil, fmt.Errorf("failed to create logger: %w", err)
	}

	// Create Telegram client
	tg, err := telegram.New(cfg.TelegramToken, cfg.Debug)
	if err != nil {
		return nil, fmt.Errorf("failed to create telegram client: %w", err)
	}

	logger.Info("Authorized as Telegram bot",
		zap.String("username", tg.GetBot().Self.UserName),
		zap.Bool("debug", cfg.Debug))

	// Create HTTP client
	httpClientConfig := &httpclient.Config{
		Timeout:       cfg.RequestTimeout,
		RetryAttempts: cfg.RetryAttempts,
		RetryMinDelay: cfg.RetryMinDelay,
		RetryMaxDelay: cfg.RetryMaxDelay,
		Debug:         cfg.Debug,
	}
	httpClient := httpclient.New(httpClientConfig, logger)

	// Create caches
	queryCache := cache.New()
	urlCache := cache.New()
	negativeCache := cache.New()

	// Create circuit breakers
	spotifyBreaker := httpclient.NewCircuitBreaker(&httpclient.CircuitBreakerConfig{
		Name:     "Spotify",
		MaxFails: cfg.CircuitBreakerMaxFails,
		Timeout:  cfg.CircuitBreakerTimeout,
	}, logger)

	youtubeBreaker := httpclient.NewCircuitBreaker(&httpclient.CircuitBreakerConfig{
		Name:     "YouTube",
		MaxFails: cfg.CircuitBreakerMaxFails,
		Timeout:  cfg.CircuitBreakerTimeout,
	}, logger)

	appleMusicBreaker := httpclient.NewCircuitBreaker(&httpclient.CircuitBreakerConfig{
		Name:     "AppleMusic",
		MaxFails: cfg.CircuitBreakerMaxFails,
		Timeout:  cfg.CircuitBreakerTimeout,
	}, logger)

	// Create platform implementations
	spotify := platform.NewSpotify(
		httpClient,
		spotifyBreaker,
		urlCache,
		negativeCache,
		&platform.SpotifyConfig{
			CacheTTL:         cfg.CacheTTL,
			NegativeCacheTTL: cfg.NegativeCacheTTL,
			TextMirrorURL:    cfg.TextMirrorURL,
			Debug:            cfg.Debug,
		},
		logger,
	)

	youtube := platform.NewYouTube(
		httpClient,
		urlCache,
		&platform.YouTubeConfig{
			CacheTTL: cfg.CacheTTL,
			Debug:    cfg.Debug,
		},
		logger,
	)

	appleMusic := platform.NewAppleMusic(
		httpClient,
		urlCache,
		&platform.AppleMusicConfig{
			CacheTTL: cfg.CacheTTL,
			Debug:    cfg.Debug,
		},
		logger,
	)

	// Create search orchestrator
	orch := search.New(spotify, youtube, appleMusic)

	return &MusicBot{
		config:            cfg,
		telegram:          tg,
		httpClient:        httpClient,
		logger:            logger,
		orchestrator:      orch,
		queryCache:        queryCache,
		urlCache:          urlCache,
		negativeCache:     negativeCache,
		spotifyBreaker:    spotifyBreaker,
		youtubeBreaker:    youtubeBreaker,
		appleMusicBreaker: appleMusicBreaker,
		fetchSem:          httpclient.NewSemaphore(cfg.MaxConcurrentFetches),
		msgSem:            httpclient.NewSemaphore(cfg.MaxConcurrentMessages),
	}, nil
}

// Run starts the bot's main loop
func (mb *MusicBot) Run(ctx context.Context) error {
	// Start cache cleanup goroutines
	go mb.queryCache.StartCleanup(ctx, 1*time.Hour)
	go mb.urlCache.StartCleanup(ctx, 1*time.Hour)
	go mb.negativeCache.StartCleanup(ctx, 10*time.Minute)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := mb.telegram.GetBot().GetUpdatesChan(u)

	log.Println("🎵 Music Link Converter Bot is running…")
	log.Println("📡 No API keys required - using web scraping!")

	for {
		select {
		case <-ctx.Done():
			log.Println("👋 Shutting down bot loop gracefully…")
			return nil
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			if update.Message == nil {
				continue
			}

			// Handle message in a goroutine with concurrency control
			go func(m *tgbotapi.Message) {
				mb.msgSem.Run(func() {
					defer func() {
						if r := recover(); r != nil {
							mb.logger.Error("Panic in handleMessage",
								zap.Any("panic", r),
								zap.Int64("chat_id", m.Chat.ID),
								zap.Int("message_id", m.MessageID))
						}
					}()

					// Create a context with timeout for message handling
					msgCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					defer cancel()

					if err := mb.handleMessage(msgCtx, m); err != nil {
						mb.logger.Error("Error handling message",
							zap.Error(err),
							zap.Int64("chat_id", m.Chat.ID),
							zap.Int("message_id", m.MessageID))
					}
				})
			}(update.Message)
		}
	}
}

// handleMessage handles a single incoming message
func (mb *MusicBot) handleMessage(ctx context.Context, m *tgbotapi.Message) error {
	chatID := m.Chat.ID
	replyTo := m.MessageID

	// Handle commands
	if m.IsCommand() {
		switch m.Command() {
		case "start":
			return mb.telegram.SendReply(chatID, replyTo,
				telegram.Md2("🎵 *Welcome to Music Link Converter!*\n\nSend me a Spotify, YouTube/YouTube Music, or Apple Music link, and I'll find it on all platforms for you.\n\n_No API keys required_"))
		case "help":
			return mb.telegram.SendReply(chatID, replyTo,
				telegram.Md2("📖 *How to use:*\n\n1. Send a music link from Spotify, YouTube/YouTube Music, or Apple Music\n2. I'll search for the same song on all platforms\n3. Get links for all three services!\n\n*Supported formats:*\n• Spotify: `https://open.spotify.com/track/...`\n• YouTube Music: `https://music.youtube.com/watch?v=...`\n• YouTube: `https://www.youtube.com/watch?v=...` or `https://youtu.be/...`\n• Apple Music: `https://music.apple.com/...`"))
		}
		return nil
	}

	// Extract song info from message
	info, err := mb.orchestrator.ExtractSongInfo(ctx, m.Text)
	if err != nil {
		mb.logger.Debug("Failed to extract song info", zap.Error(err))
		return mb.telegram.SendReply(chatID, replyTo,
			telegram.Md2("❌ Please send a valid *music link* from Spotify, YouTube/YouTube Music, or Apple Music."))
	}

	if info == nil {
		return mb.telegram.SendReply(chatID, replyTo,
			telegram.Md2("❌ Please send a valid *music link* from Spotify, YouTube/YouTube Music, or Apple Music."))
	}

	// Send initial response
	artist := info.Artist
	if strings.TrimSpace(artist) == "" {
		artist = "Unknown Artist"
	}

	if err := mb.telegram.SendReply(chatID, replyTo,
		telegram.Md2(fmt.Sprintf("🔍 Found: *%s* by *%s*\n\nSearching other platforms…", info.Title, artist))); err != nil {
		mb.logger.Error("Failed to send initial reply", zap.Error(err))
	}

	// Search on all platforms
	links := mb.orchestrator.FindOnAllPlatforms(ctx, info)

	// Enrich missing artist using other platform results
	mb.orchestrator.EnrichArtistInfo(ctx, info, links)

	// Check if we found any links
	if links.Spotify == "" && links.YouTubeMusic == "" && links.AppleMusic == "" {
		return mb.telegram.SendReply(chatID, replyTo,
			telegram.Md2("😕 I couldn't find matches on other platforms. It might be a regional or rare release."))
	}

	// Send results
	return mb.telegram.SendLinks(chatID, replyTo, info, links)
}

// createLogger creates a zap logger
func createLogger(debug bool) (*zap.Logger, error) {
	if debug {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}
