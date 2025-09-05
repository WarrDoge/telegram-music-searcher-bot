# Telegram Music Link Converter Bot (No API Keys Version)

A simple Telegram bot that converts music links between Spotify, YouTube Music, and Apple Music using web scraping. **No API keys required** (except Telegram)!

## 🎯 Features

- 🎵 Accepts music links from Spotify, YouTube Music, and Apple Music
- 🔍 Extracts song information using web scraping
- 📱 Provides search links for all three platforms
- ⚡ Fast and lightweight
- 🔓 **No API credentials needed** for music platforms
- 🤖 Simple Telegram interface

## 📋 Prerequisites

- Go 1.21 or higher
- Telegram Bot Token (the only credential you need!)

## 🚀 Quick Setup

### 1. Get a Telegram Bot Token

1. Open Telegram and search for [@BotFather](https://t.me/botfather)
2. Send `/newbot` and follow the instructions
3. Copy your bot token

### 2. Clone and Setup

```bash
# Create project directory
mkdir telegram-music-bot
cd telegram-music-bot

# Copy the main.go and go.mod files here
# Then install dependencies
go mod download
```

### 3. Run the Bot

```bash
# Set your bot token and run
export TELEGRAM_BOT_TOKEN="your_bot_token_here"
go run main.go
```

Or create a simple run script `run.sh`:

```bash
#!/bin/bash
export TELEGRAM_BOT_TOKEN="your_bot_token_here"
go run main.go
```

## 💬 Usage

1. Start a conversation with your bot on Telegram
2. Send `/start` to see the welcome message
3. Send a music link from any supported platform
4. The bot will:
   - Extract the song title and artist from the link
   - Generate search links for the other platforms
   - Return clickable links that take you directly to search results

### Supported Link Formats

- **Spotify**: `https://open.spotify.com/track/...`
- **YouTube Music**: `https://music.youtube.com/watch?v=...`
- **Apple Music**: `https://music.apple.com/.../album/...`

### Bot Commands

- `/start` - Welcome message and introduction
- `/help` - Show usage instructions

## 🔧 How It Works

The bot uses **web scraping** instead of APIs:

1. **Extracts Song Info**: 
   - Spotify: Uses OEmbed endpoint (no auth) or scrapes meta tags
   - YouTube: Scrapes Open Graph meta tags
   - Apple Music: Scrapes meta tags and structured data

2. **Generates Search Links**:
   - Creates pre-filled search URLs for each platform
   - When clicked, these links show the song as the first result
   - No complex API authentication needed!

## 🐳 Docker Deployment

Create a `Dockerfile`:

```dockerfile
FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN go build -o bot main.go

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/bot .
CMD ["./bot"]
```

Build and run:

```bash
docker build -t music-bot .
docker run -e TELEGRAM_BOT_TOKEN="your_token" music-bot
```

## 🌐 Deploy to Cloud Platforms

### Heroku

Create `Procfile`:
```
worker: ./bot
```

Deploy:
```bash
heroku create your-music-bot
heroku config:set TELEGRAM_BOT_TOKEN="your_token"
git push heroku main
heroku ps:scale worker=1
```

### Railway.app

1. Push code to GitHub
2. Connect Railway to your repo
3. Add environment variable `TELEGRAM_BOT_TOKEN`
4. Deploy!

### Fly.io

Create `fly.toml`:
```toml
app = "music-link-bot"

[env]
  TELEGRAM_BOT_TOKEN = "your_token"

[[services]]
  internal_port = 8080
  protocol = "tcp"
```

Deploy:
```bash
fly launch
fly deploy
```

## ⚠️ Limitations

Since this version uses web scraping instead of official APIs:

1. **Search Links vs Direct Links**: For cross-platform searches, the bot provides search URLs that will show the song as the first result, rather than direct track links

2. **Scraping Reliability**: Web scraping can break if websites change their structure

3. **No Advanced Features**: Can't access features like:
   - Album artwork
   - Track duration
   - Preview URLs
   - Playlist conversion

4. **Rate Limiting**: Excessive requests might get temporarily blocked by the platforms

## 🔄 How Search Links Work

When you search for a song on another platform, the bot generates a search URL like:
- Spotify: `https://open.spotify.com/search/Artist%20Song%20Title`
- YouTube Music: `https://music.youtube.com/search?q=Artist%20Song%20Title`
- Apple Music: `https://music.apple.com/search?term=Artist%20Song%20Title`

These links will open the search page with the song typically appearing as the first result.

## 🛠️ Improvements You Can Add

1. **Enhanced Scraping**: Add more robust HTML parsing patterns
2. **Caching**: Cache extracted song info to reduce requests
3. **Direct Link Search**: Implement Google search scraping to find direct links
4. **Fuzzy Matching**: Improve search accuracy with string similarity algorithms
5. **Error Recovery**: Add retry logic for failed requests
6. **User Stats**: Track usage statistics
7. **Inline Mode**: Add Telegram inline query support

## 🐛 Troubleshooting

### Bot not responding
- Check if the bot is running
- Verify your Telegram token is correct
- Check console logs for errors

### Can't extract song info
- The website structure might have changed
- Try with a different song link
- Check if the link is accessible in your region

### Search links not working
- Make sure you're logged into the platform
- The song might not be available in your region
- Try searching manually with the artist and title

## 📝 Example Usage

```
User: https://open.spotify.com/track/3n3Ppam7vgaVa1iaRUc9Lp

Bot: 🎵 Music Links Found!

🎼 Title: Mr. Brightside
🎤 Artist: The Killers

📱 Available on:

✅ Spotify - Direct Link
🔍 YouTube Music - Click to Search
🔍 Apple Music - Click to Search

💡 Tip: Search links will show the song as the first result
```

## 🤝 Contributing

Feel free to improve the bot by:
- Adding better scraping patterns
- Supporting more platforms (Deezer, Tidal, etc.)
- Improving song matching accuracy
- Adding new features

## 📄 License

This project is provided as-is for educational purposes.

## ⚡ Benefits of This Approach

- ✅ **Super Simple Setup**: Just one token needed
- ✅ **No API Costs**: Everything is free
- ✅ **No Rate Limits**: No official API quotas
- ✅ **Privacy**: No data sent to third-party APIs
- ✅ **Lightweight**: Minimal dependencies

## 🎉 That's It!

You now have a working music link converter bot with just a Telegram token! No complex API setup, no authentication headaches, just simple web scraping that works.
