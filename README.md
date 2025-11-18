# Telegram Music Link Converter Bot

A Telegram bot that converts music links between Spotify, YouTube Music, and Apple Music using web scraping. No API keys required (except Telegram Bot Token).

## Features

- Convert music links between 3 major platforms
- 6-layer fallback extraction strategy for robust parsing
- Smart fuzzy matching and text normalization
- Circuit breaker pattern for API reliability
- Positive and negative caching
- DuckDuckGo search fallback
- No platform API keys needed

## Quick Start

### Prerequisites

- Go 1.24+
- Telegram Bot Token (get from [@BotFather](https://t.me/botfather))
- Linux server (for deployment)

### Installation

1. Clone the repository:

```bash
git clone <repo-url>
cd telegram-music-searcher-bot
```

2. Create `.env` file:

```bash
cp .env.example .env
# Edit .env and add your Telegram bot token
```

3. Build and run:

```bash
make deps    # Install dependencies
make test    # Run tests
make build   # Build binary
./telegram-bot
```

## Environment Variables

Create a `.env` file with the following variables:

```bash
# Required
TOKEN=your_telegram_bot_token_here

# Optional - for deployment
APP=telegram-bot
SSH_KEY=/path/to/ssh/key
IP=your.server.ip
USER=your_username
```

## Usage

Send any Spotify, YouTube Music, or Apple Music link to the bot:

```
https://open.spotify.com/track/...
https://music.youtube.com/watch?v=...
https://music.apple.com/us/album/...
```

The bot will respond with equivalent links for all platforms.

## Development

### Project Structure

- `main.go` - Main application (monolith architecture)
- `main_test.go` - Unit and integration tests
- `Makefile` - Build and deployment automation
- `service.tpl` - SystemD service template

### Running Tests

```bash
# All tests
make test
```

### Deployment

Configure `.env` with SSH details, then:

```bash
make deploy
```

This will:

1. Run tests
2. Build static binary
3. Deploy to remote server via SSH
4. Install as SystemD service
5. Start the bot

## Architecture

### Extraction Strategy

The bot uses a 6-layer fallback approach for Spotify:

1. OEmbed API (official)
2. Open Graph tags
3. JSON-LD structured data
4. HTML title parsing
5. DuckDuckGo search
6. Negative caching

YouTube and Apple Music use similar multi-layer strategies.

### Components

- **HTTP Client**: Retry logic with exponential backoff
- **Circuit Breaker**: Auto-recovery from platform failures
- **Cache**: TTL-based with positive/negative caching
- **Text Normalizer**: Handles various title formats
- **Fuzzy Matcher**: Levenshtein distance for similarity
- **Search Orchestrator**: Manages platform requests concurrently

## Contributing

See [TODO.md](TODO.md) for planned improvements and open tasks.

## License

GNU GENERAL PUBLIC LICENSE Version 3, 29 June 2007

## Troubleshooting

### Bot doesn't respond

- Check bot token is valid
- Ensure bot is running: `systemctl status telegram-bot`
- Check logs: `journalctl -u telegram-bot -f`

### Link extraction fails

- Platform may have changed their HTML structure
- Check circuit breaker status (auto-recovers after 30s)
- Verify network connectivity to music platforms

### Tests failing

- Integration tests require internet connection
- Use `make test` for offline testing
- Ensure Go 1.24+ is installed

## Support

For issues or feature requests, check the TODO.md for planned improvements or open a new issue.
