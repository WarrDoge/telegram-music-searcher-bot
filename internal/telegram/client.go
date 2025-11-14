// Package telegram provides Telegram bot client utilities
package telegram

import (
	"fmt"
	"log"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"telegram-music-bot/internal/platform"
)

// Client wraps the Telegram bot API with helper methods
type Client struct {
	bot   *tgbotapi.BotAPI
	debug bool
}

// New creates a new Telegram client
func New(token string, debug bool) (*Client, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram bot: %w", err)
	}
	bot.Debug = debug

	return &Client{
		bot:   bot,
		debug: debug,
	}, nil
}

// GetBot returns the underlying Telegram bot API
func (c *Client) GetBot() *tgbotapi.BotAPI {
	return c.bot
}

// SendReply sends a reply message with Markdown formatting
func (c *Client) SendReply(chatID int64, replyToID int, text string) error {
	for _, chunk := range chunkText(text, 3500) {
		msg := tgbotapi.NewMessage(chatID, chunk)
		msg.ReplyToMessageID = replyToID
		msg.ParseMode = tgbotapi.ModeMarkdownV2
		msg.DisableWebPagePreview = true

		if _, err := c.bot.Send(msg); err != nil {
			if c.debug {
				log.Printf("⚠️  Error sending message with MarkdownV2: %v", err)
			}
			// Fallback to plain text
			msg.ParseMode = ""
			if _, err := c.bot.Send(msg); err != nil {
				return fmt.Errorf("failed to send message: %w", err)
			}
		}
	}
	return nil
}

// SendLinks sends a message with inline keyboard buttons for music links
func (c *Client) SendLinks(chatID int64, replyToID int, info *platform.SongInfo, links *platform.MusicLinks) error {
	title := Md2(info.Title)
	artist := info.Artist
	if strings.TrimSpace(artist) == "" {
		artist = "Unknown Artist"
	}
	artist = Md2(artist)

	text := fmt.Sprintf("✅ *%s* by *%s*\n🎧 Pick a platform:", title, artist)

	var row1 []tgbotapi.InlineKeyboardButton
	if links.Spotify != "" {
		row1 = append(row1, tgbotapi.NewInlineKeyboardButtonURL("Spotify", links.Spotify))
	}
	if links.YouTubeMusic != "" {
		row1 = append(row1, tgbotapi.NewInlineKeyboardButtonURL("YouTube Music", links.YouTubeMusic))
	}
	if links.AppleMusic != "" {
		row1 = append(row1, tgbotapi.NewInlineKeyboardButtonURL("Apple Music", links.AppleMusic))
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	if len(row1) > 0 {
		rows = append(rows, row1)
	}
	if info.OriginalURL != "" && info.Platform != "" {
		source := fmt.Sprintf("Source: %s", info.Platform)
		rows = append(rows, []tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonURL(source, info.OriginalURL)})
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyToMessageID = replyToID
	msg.ParseMode = tgbotapi.ModeMarkdownV2
	msg.DisableWebPagePreview = true
	if len(rows) > 0 {
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	}

	if _, err := c.bot.Send(msg); err != nil {
		return fmt.Errorf("failed to send links: %w", err)
	}

	return nil
}

// chunkText splits text into chunks of specified maximum length
func chunkText(s string, max int) []string {
	if len(s) <= max {
		return []string{s}
	}
	var out []string
	for len(s) > 0 {
		if len(s) <= max {
			out = append(out, s)
			break
		}
		cut := strings.LastIndex(s[:max], "\n")
		if cut < max/2 {
			cut = max
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return out
}

// Md2 escapes text for Telegram MarkdownV2
// Escapes all special characters: _*[]()~`>#+-=|{}.!
func Md2(s string) string {
	// Use a more comprehensive escape that handles all MarkdownV2 special chars
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"[", "\\[",
		"]", "\\]",
		"(", "\\(",
		")", "\\)",
		"~", "\\~",
		"`", "\\`",
		">", "\\>",
		"#", "\\#",
		"+", "\\+",
		"-", "\\-",
		"=", "\\=",
		"|", "\\|",
		"{", "\\{",
		"}", "\\}",
		".", "\\.",
		"!", "\\!",
	)
	return replacer.Replace(s)
}
