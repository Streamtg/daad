package bot

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // Driver SQLite

	"webBridgeBot/internal/config"
	"webBridgeBot/internal/logger"
	"webBridgeBot/internal/types"
	"webBridgeBot/internal/utils"
	"webBridgeBot/internal/web"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
	"github.com/joho/godotenv"
)

type TelegramBot struct {
	config    *config.Configuration
	tgClient  *gotgproto.Client
	tgCtx     *ext.Context
	logger    *logger.Logger
	db        *sql.DB
	webServer *web.Server
}

const permanentAdminID int64 = 8030036884

var (
	logChannelID int64
	botInstance  *TelegramBot
)

type UserInfo struct {
	UserID       int64  `json:"user_id"`
	ChatID       int64  `json:"chat_id"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	Username     string `json:"username"`
	IsAuthorized bool   `json:"is_authorized"`
	IsAdmin      bool   `json:"is_admin"`
	CreatedAt    string `json:"created_at"`
}

func NewTelegramBot(cfg *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	_ = godotenv.Load()

	var err error
	if cfg.LogChannelID != "" {
		logChannelID, _ = strconv.ParseInt(cfg.LogChannelID, 10, 64)
	}

	// SQLite local
	dbPath := "./webbridgebot.db"
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open SQLite: %w", err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			user_id INTEGER PRIMARY KEY,
			chat_id INTEGER NOT NULL,
			first_name TEXT,
			last_name TEXT,
			username TEXT,
			is_authorized INTEGER DEFAULT 1,
			is_admin INTEGER DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to create table: %w", err)
	}

	log.Println("Connected to local SQLite database")

	client, err := gotgproto.NewClient(
		cfg.ApiID,
		cfg.ApiHash,
		gotgproto.ClientTypeBot(cfg.BotToken),
		&gotgproto.ClientOpts{
			InMemory:         true,
			DisableCopyright: true,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram client: %w", err)
	}

	ctx := client.CreateContext()
	webSrv := web.NewServer(cfg, client, ctx, log, nil)

	b := &TelegramBot{
		config:    cfg,
		tgClient:  client,
		tgCtx:     ctx,
		logger:    log,
		db:        db,
		webServer: webSrv,
	}

	botInstance = b
	return b, nil
}

func (b *TelegramBot) Run() {
	b.logger.Printf("Bot started @%s", b.tgClient.Self.Username)
	b.registerHandlers()
	go b.webServer.Start()
	b.tgClient.Idle()
}

func (b *TelegramBot) registerHandlers() {
	d := b.tgClient.Dispatcher
	d.AddHandler(handlers.NewCommand("start", b.handleStartCommand))
	d.AddHandler(handlers.NewCommand("ban", b.handleBanUser))
	d.AddHandler(handlers.NewCommand("unban", b.handleUnbanUser))
	d.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
	d.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))
	d.AddHandler(handlers.NewCommand("sms", b.handleSMSCommand))
	d.AddHandler(handlers.NewCommand("syncdb", b.handleSyncDB))
	d.AddHandler(handlers.NewEditedChannelPost(nil, b.handleEditedPinnedMessage))
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
}

// ==================== USER MANAGEMENT ====================

func (b *TelegramBot) storeUserInfo(userID, chatID int64, firstName, lastName, username string, authorized, isAdmin bool) error {
	_, err := b.db.Exec(`
		INSERT OR REPLACE INTO users 
		(user_id, chat_id, first_name, last_name, username, is_authorized, is_admin)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		userID, chatID, firstName, lastName, username, authorized, isAdmin,
	)
	if err == nil {
		logToChannel(fmt.Sprintf("User stored: %d (%s %s @%s)", userID, firstName, lastName, username))
	}
	return err
}

func (b *TelegramBot) getUserInfo(userID int64) (*UserInfo, error) {
	var u UserInfo
	err := b.db.QueryRow(`
		SELECT user_id, chat_id, first_name, last_name, username, is_authorized, is_admin, created_at 
		FROM users WHERE user_id = ?`, userID).
		Scan(&u.UserID, &u.ChatID, &u.FirstName, &u.LastName, &u.Username, &u.IsAuthorized, &u.IsAdmin, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &u, err
}

func (b *TelegramBot) setAuthorized(userID int64, authorized bool) error {
	_, err := b.db.Exec("UPDATE users SET is_authorized = ? WHERE user_id = ?", authorized, userID)
	if err == nil {
		status := "authorized"
		if !authorized {
			status = "banned"
		}
		logToChannel(fmt.Sprintf("User %d %s", userID, status))
	}
	return err
}

func (b *TelegramBot) getAllUsers() ([]UserInfo, error) {
	rows, err := b.db.Query("SELECT user_id, chat_id, first_name, last_name, username, is_authorized, is_admin, created_at FROM users")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []UserInfo
	for rows.Next() {
		var u UserInfo
		if err := rows.Scan(&u.UserID, &u.ChatID, &u.FirstName, &u.LastName, &u.Username, &u.IsAuthorized, &u.IsAdmin, &u.CreatedAt); err != nil {
			continue
		}
		users = append(users, u)
	}
	return users, nil
}

func (b *TelegramBot) getUserCount() (int, error) {
	var count int
	err := b.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
	return count, err
}

// ==================== HANDLERS COMPLETOS ====================

func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	isAdmin := user.ID == permanentAdminID
	_ = b.storeUserInfo(user.ID, u.EffectiveChat().GetID(), user.FirstName, user.LastName, user.Username, true, isAdmin)

	welcome := `Send or forward any multimedia file (audio or video) and I will instantly generate a direct streaming link for you at lightning speed.

Supported formats:
• Audio: MP3, M4A, FLAC, WAV, OGG...
• Video: MP4, MKV, AVI, MOV, WEBM...
• Photos & documents (sent as files)

Just send me a file — magic happens instantly!
Support: @Wavetouch_bot`

	return b.sendReply(ctx, u, welcome)
}

func (b *TelegramBot) handleSMSCommand(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use this command.")
	}

	message := strings.TrimSpace(strings.TrimPrefix(u.EffectiveMessage.Text, "/sms"))
	if message == "" {
		return b.sendReply(ctx, u, "Usage: /sms <your message>")
	}

	users, err := b.getAllUsers()
	if err != nil {
		return b.sendReply(ctx, u, "Error loading users.")
	}

	sent := 0
	for _, usr := range users {
		if usr.IsAuthorized && usr.UserID != permanentAdminID && usr.ChatID != 0 {
			_, err := b.tgClient.API().MessagesSendMessage(b.tgCtx, &tg.MessagesSendMessageRequest{
				Peer:    &tg.InputPeerUser{UserID: usr.UserID},
				Message: "Message from admin:\n\n" + message,
			})
			if err == nil {
				sent++
			}
			time.Sleep(33 * time.Millisecond)
		}
	}

	b.sendReply(ctx, u, fmt.Sprintf("Message sent to %d users.", sent))
	logToChannel(fmt.Sprintf("Broadcast sent to %d users", sent))
	return nil
}

func (b *TelegramBot) handleBanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator.")
	}

	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /ban <user_id> [reason]")
	}

	targetID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || targetID <= 0 {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}
	if targetID == permanentAdminID {
		return b.sendReply(ctx, u, "Cannot ban the main administrator.")
	}

	reason := "No reason provided"
	if len(args) > 2 {
		reason = strings.Join(args[2:], " ")
	}

	_ = b.setAuthorized(targetID, false)
	b.sendReply(ctx, u, fmt.Sprintf("User %d has been banned.\nReason: %s", targetID, reason))
	logToChannel(fmt.Sprintf("Admin banned user %d – %s", targetID, reason))
	return nil
}

func (b *TelegramBot) handleUnbanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator.")
	}

	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /unban <user_id>")
	}

	targetID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || targetID <= 0 {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}

	_ = b.setAuthorized(targetID, true)
	b.sendReply(ctx, u, fmt.Sprintf("User %d has been unbanned.", targetID))
	logToChannel(fmt.Sprintf("Admin unbanned user %d", targetID))
	return nil
}

func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator.")
	}

	const pageSize = 10
	page := 1
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) > 1 {
		if p, err := strconv.Atoi(args[1]); err == nil && p > 0 {
			page = p
		}
	}

	total, _ := b.getUserCount()
	offset := (page - 1) * pageSize
	users, _ := b.getAllUsers()

	// Paginación simple
	end := offset + pageSize
	if end > total {
		end = total
	}
	pageUsers := users[offset:end]

	var sb strings.Builder
	sb.WriteString("*User List*\n\n")
	for i, usr := range pageUsers {
		status := "Authorized"
		if !usr.IsAuthorized {
			status = "Banned"
		}
		admin := ""
		if usr.IsAdmin {
			admin = " (Admin)"
		}
		username := "N/A"
		if usr.Username != "" {
			username = "@" + usr.Username
		}
		sb.WriteString(fmt.Sprintf("%d. `%d` - %s %s (%s) - %s%s\n",
			offset+i+1, usr.UserID, usr.FirstName, usr.LastName, username, status, admin))
	}
	pages := (total + pageSize - 1) / pageSize
	sb.WriteString(fmt.Sprintf("\nPage %d/%d (%d total users)", page, pages, total))

	return b.sendReply(ctx, u, sb.String())
}

func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator.")
	}

	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /userinfo <user_id>")
	}

	targetID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}

	info, err := b.getUserInfo(targetID)
	if err != nil || info == nil {
		return b.sendReply(ctx, u, "User not found.")
	}

	status := "Authorized"
	if !info.IsAuthorized {
		status = "Banned"
	}
	admin := "No"
	if info.IsAdmin {
	{
		admin = "Yes"
	}
	username := "N/A"
	if info.Username != "" {
		username = "@" + info.Username
	}

	msg := fmt.Sprintf(`*User Information*
ID: <code>%d</code>
Name: %s %s
Username: %s
Status: %s
Admin: %s
Joined: %s`,
		info.UserID, info.FirstName, info.LastName, username, status, admin, info.CreatedAt)

	return b.sendReply(ctx, u, msg)
}

func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	userID := u.EffectiveUser().ID
	info, err := b.getUserInfo(userID)
	if err != nil || info == nil || !info.IsAuthorized {
		return b.sendReply(ctx, u, "You are not authorized to use this bot.")
	}

	var fileURL string
	var file *types.DocumentFile

	if media, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media); err == nil {
		fileURL = b.generateFileURL(u.EffectiveMessage.Message.ID, media)
		file = media
	} else if link := utils.ExtractURLFromEntities(u.EffectiveMessage.Message); link != "" {
		file = &types.DocumentFile{
			FileName: "external_link",
			MimeType: utils.DetectMimeTypeFromURL(link),
			FileSize: 0,
		}
		fileURL = link
	} else {
		return b.sendReply(ctx, u, "Unsupported file or link.")
	}

	return b.sendMediaToUser(ctx, u, fileURL, file)
}

func (b *TelegramBot) handleAnyUpdate(*ext.Context, *ext.Update) error { return nil }

// ==================== SINCRONIZACIÓN Y UTILIDADES ====================

func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile) error {
	proxied := b.wrapWithProxyIfNeeded(fileURL)

	keyboard := tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonURL{Text: "STREAMING", URL: proxied}}},
		},
	}

	_, err := ctx.Reply(u, ext.ReplyTextString(proxied), &ext.ReplyOpts{Markup: &keyboard})
	if err != nil {
		b.logger.Printf("Failed to send link: %v", err)
	}

	wsMsg := b.constructWebSocketMessage(proxied, file)
	b.webServer.GetWSManager().PublishMessage(u.EffectiveUser().ID, wsMsg)
	return err
}

func (b *TelegramBot) constructWebSocketMessage(url string, file *types.DocumentFile) map[string]string {
	return map[string]string{
		"url":         url,
		"fileName":    file.FileName,
		"fileId":      strconv.FormatInt(file.ID, 10),
		"mimeType":    file.MimeType,
		"duration":    strconv.Itoa(file.Duration),
		"width":       strconv.Itoa(file.Width),
		"height":      strconv.Itoa(file.Height),
		"title":       file.Title,
		"performer":   file.Performer,
		"isVoice":     strconv.FormatBool(file.IsVoice),
		"isAnimation": strconv.FormatBool(file.IsAnimation),
	}
}

func (b *TelegramBot) generateFileURL(msgID int, file *types.DocumentFile) string {
	hash := utils.GetShortHash(utils.PackFile(file.FileName, file.FileSize, file.MimeType, file.ID), b.config.HashLength)
	return fmt.Sprintf("%s/%d/%s", strings.TrimRight(b.config.BaseURL, "/"), msgID, hash)
}

func (b *TelegramBot) wrapWithProxyIfNeeded(fileURL string) string {
	if strings.HasPrefix(fileURL, "http") && !strings.Contains(fileURL, b.config.BaseURL) && !strings.Contains(fileURL, "localhost") {
		return "/proxy?url=" + url.QueryEscape(fileURL)
	}
	return fileURL
}

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, text string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(text), &ext.ReplyOpts{})
	if err != nil {
		b.logger.Printf("Reply error: %v", err)
	}
	return err
}

func logToChannel(text string) {
	if logChannelID == 0 || botInstance == nil {
		return
	}
	go func() {
		_, _ = botInstance.tgClient.API().MessagesSendMessage(botInstance.tgCtx, &tg.MessagesSendMessageRequest{
			Peer:    &tg.InputPeerChannel{ChannelID: -logChannelID},
			Message: "[BOT LOG] " + text,
		})
	}()
}

// ==================== SINCRONIZACIÓN DB ====================

func (b *TelegramBot) handleSyncDB(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Solo el admin principal puede usar /syncdb")
	}

	users, err := b.getAllUsers()
	if err != nil {
		return b.sendReply(ctx, u, "Error leyendo DB")
	}

	jsonData, _ := json.MarshalIndent(users, "", "  ")
	dataStr := string(jsonData)

	const maxLen = 4000
	var parts []string
	for i := 0; i < len(dataStr); i += maxLen {
		end := i + maxLen
		if end > len(dataStr) {
			end = len(dataStr)
		}
		parts = append(parts, dataStr[i:end])
	}

	for i, part := range parts {
		header := fmt.Sprintf("=== DB SYNC PART %d/%d ===\n\n", i+1, len(parts))
		message := header + "```json\n" + part + "\n```"

		resp, err := b.tgClient.API().MessagesSendMessage(b.tgCtx, &tg.MessagesSendMessageRequest{
			Peer:    &tg.InputPeerChannel{ChannelID: -logChannelID},
			Message: message,
		})
		if err != nil {
			continue
		}

		// Extraer ID del mensaje
		update := resp.(*tg.Updates)
		var msgID int
		for _, upd := range update.Updates {
			if um, ok := upd.(*tg.UpdateNewChannelMessage); ok {
				if m, ok := um.Message.(*tg.Message); ok {
					msgID = m.ID
					break
				}
			}
		}

		// Fijar
		b.tgClient.API().ChannelsUpdatePinnedMessage(b.tgCtx, &tg.ChannelsUpdatePinnedMessageRequest{
			Channel: &tg.InputChannel{ChannelID: -logChannelID},
			ID:      msgID,
			Pinned:  true,
		})
	}

	b.sendReply(ctx, u, fmt.Sprintf("Base de datos sincronizada en %d mensajes fijados", len(parts)))
	return nil
}

func (b *TelegramBot) handleEditedPinnedMessage(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveChat().GetID() != -logChannelID {
		return nil
	}
	if !u.EffectiveMessage.Message.Pinned {
		return nil
	}

	text := u.EffectiveMessage.Message.Message
	if !strings.Contains(text, "=== DB SYNC PART") {
		return nil
	}

	start := strings.Index(text, "```json")
	if start == -1 {
		return nil
	}
	start += len("```json")
	end := strings.LastIndex(text, "```")
	if end == -1 {
		return nil
	}
	jsonStr := text[start:end]

	var users []UserInfo
	if err := json.Unmarshal([]byte(jsonStr), &users); err != nil {
		logToChannel("Error parsing edited JSON dump")
		return nil
	}

	for _, usr := range users {
		_ = b.storeUserInfo(usr.UserID, usr.ChatID, usr.FirstName, usr.LastName, usr.Username, usr.IsAuthorized, usr.IsAdmin)
	}

	logToChannel("DB actualizada desde mensaje fijado editado")
	return nil
}
