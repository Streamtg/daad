package bot

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"webBridgeBot/internal/config"
	"webBridgeBot/internal/data"
	"webBridgeBot/internal/logger"
	"webBridgeBot/internal/types"
	"webBridgeBot/internal/utils"
	"webBridgeBot/internal/web"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/glebarez/sqlite"
	"github.com/gotd/td/tg"
)

type TelegramBot struct {
	config         *config.Configuration
	tgClient       *gotgproto.Client
	tgCtx          *ext.Context
	logger         *logger.Logger
	userRepository *data.UserRepository
	db             *sql.DB
	webServer      *web.Server
	logChannelID   int64
}

const permanentAdminID int64 = 8030036884

// NewTelegramBot crea el bot y, si no existe DB local, intenta restaurarla del canal
func NewTelegramBot(cfg *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	dsn := fmt.Sprintf("file:%s?mode=rwc", cfg.DatabasePath)

	if _, err := os.Stat(cfg.DatabasePath); os.IsNotExist(err) {
		log.Printf("Local DB not found, trying to download last backup…")
		if err := downloadDBFromLogChannel(cfg, log); err != nil {
			log.Printf("Could not download backup (%v), starting with empty DB", err)
		}
	}

	tgClient, err := gotgproto.NewClient(
		cfg.ApiID,
		cfg.ApiHash,
		gotgproto.ClientTypeBot(cfg.BotToken),
		&gotgproto.ClientOpts{
			InMemory:          true,
			Session:           sessionMaker.SqlSession(sqlite.Open(dsn)),
			DisableCopyright: true,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Telegram client: %w", err)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open SQLite database: %w", err)
	}

	userRepo := data.NewUserRepository(db)
	if err := userRepo.InitDB(); err != nil {
		return nil, err
	}

	tgCtx := tgClient.CreateContext()
	webServer := web.NewServer(cfg, tgClient, tgCtx, log, userRepo)

	logChannelID, _ := strconv.ParseInt(cfg.LogChannelID, 10, 64)

	return &TelegramBot{
		config:         cfg,
		tgClient:       tgClient,
		tgCtx:          tgCtx,
		logger:         log,
		userRepository: userRepo,
		db:             db,
		webServer:      webServer,
		logChannelID:   logChannelID,
	}, nil
}

func (b *TelegramBot) Run() {
	b.logger.Printf("Starting Telegram bot (@%s)…\n", b.tgClient.Self.Username)
	b.registerHandlers()
	go b.webServer.Start()
	if err := b.tgClient.Idle(); err != nil {
		b.logger.Fatalf("Failed to start Telegram client: %s", err)
	}
}

func (b *TelegramBot) registerHandlers() {
	d := b.tgClient.Dispatcher
	d.AddHandler(handlers.NewCommand("start", b.handleStartCommand))
	d.AddHandler(handlers.NewCommand("ban", b.handleBanUser))
	d.AddHandler(handlers.NewCommand("unban", b.handleUnbanUser))
	d.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
	d.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))
	d.AddHandler(handlers.NewCommand("sms", b.handleSMS))
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
}

// ==================== /start ====================
func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}
	isAuthorized := true
	isAdmin := user.ID == permanentAdminID
	if err := b.userRepository.StoreUserInfo(
		user.ID,
		u.EffectiveChat().GetID(),
		user.FirstName,
		user.LastName,
		user.Username,
		isAuthorized,
		isAdmin,
	); err != nil {
		b.logger.Printf("Failed to store user %d: %v", user.ID, err)
		return err
	}
	welcome := `Send or forward any multimedia file (audio or video) and I will instantly generate a direct streaming link for you at lightning speed.

Supported formats:
• Audio: MP3, M4A, FLAC, WAV, OGG...
• Video: MP4, MKV, AVI, MOV, WEBM...
• Photos & documents (sent as files)

How to use me:
• Personal media host (movies, series, documentaries)
• Share large videos without Telegram compression
• Build your private streaming library
• Stream directly in browser from any device
• Access your files anywhere, anytime

Just send me a file — magic happens instantly! 
Support: @Wavetouch_bot`
	return b.sendReply(ctx, u, welcome)
}

// ==================== /ban ====================
func (b *TelegramBot) handleBanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use this command.")
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
		return b.sendReply(ctx, u, "You cannot ban the main administrator.")
	}
	reason := "No reason provided"
	if len(args) > 2 {
		reason = strings.Join(args[2:], " ")
	}
	if err := b.userRepository.DeauthorizeUser(targetID); err != nil {
		return b.sendReply(ctx, u, "Failed to ban user.")
	}
	b.logger.Printf("ADMIN %d banned user %d – Reason: %s", permanentAdminID, targetID, reason)
	go b.uploadDBToLogChannel("DB backup – ban user")
	go func() {
		info, _ := b.userRepository.GetUserInfo(targetID)
		if info != nil && info.ChatID != 0 {
			peer := b.tgCtx.PeerStorage.GetInputPeerById(info.ChatID)
			b.tgCtx.SendMessage(info.ChatID, &tg.MessagesSendMessageRequest{
				Peer:    peer,
				Message: fmt.Sprintf("You have been permanently banned from using this bot.\nSupport: @Wavetouch_bot\n\nReason: %s", reason),
			})
		}
	}()
	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been banned.\nSupport: @Wavetouch_bot\nReason: %s", targetID, reason))
}

// ==================== /unban ====================
func (b *TelegramBot) handleUnbanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the administrator can use this command.")
	}
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /unban <user_id>")
	}
	targetID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || targetID <= 0 {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}
	if err := b.userRepository.AuthorizeUser(targetID, false); err != nil {
		return b.sendReply(ctx, u, "Failed to unban user.")
	}
	b.logger.Printf("ADMIN %d unbanned user %d", permanentAdminID, targetID)
	go b.uploadDBToLogChannel("DB backup – unban user")
	go func() {
		info, _ := b.userRepository.GetUserInfo(targetID)
		if info != nil && info.ChatID != 0 {
			peer := b.tgCtx.PeerStorage.GetInputPeerById(info.ChatID)
			b.tgCtx.SendMessage(info.ChatID, &tg.MessagesSendMessageRequest{
				Peer:    peer,
				Message: "You have been unbanned!\nYou can now use the bot again.",
			})
		}
	}()
	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been unbanned.", targetID))
}

// ==================== /listusers ====================
func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the administrator can use this command.")
	}
	const pageSize = 10
	page := 1
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) > 1 {
		if p, err := strconv.Atoi(args[1]); err == nil && p > 0 {
			page = p
		}
	}
	total, _ := b.userRepository.GetUserCount()
	if total == 0 {
		return b.sendReply(ctx, u, "No users registered yet.")
	}
	offset := (page - 1) * pageSize
	users, _ := b.userRepository.GetAllUsers(offset, pageSize)
	if len(users) == 0 {
		return b.sendReply(ctx, u, "No users on this page.")
	}
	var msg strings.Builder
	msg.WriteString("*User List*\n\n")
	for i, usr := range users {
		status := "Authorized"
		if !usr.IsAuthorized {
			status = "Banned"
		}
		adminTag := ""
		if usr.IsAdmin {
			adminTag = " (Admin)"
		}
		username := usr.Username
		if username == "" {
			username = "N/A"
		}
		msg.WriteString(fmt.Sprintf("%d. `%d` – %s %s (@%s) – %s%s\n",
			offset+i+1, usr.UserID, usr.FirstName, usr.LastName, username, status, adminTag))
	}
	totalPages := (total + pageSize - 1) / pageSize
	msg.WriteString(fmt.Sprintf("\nPage %d of %d (%d total users)", page, totalPages, total))
	return b.sendReply(ctx, u, msg.String())
}

// ==================== /userinfo ====================
func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the administrator can use this command.")
	}
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /userinfo <user_id>")
	}
	targetID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}
	target, err := b.userRepository.GetUserInfo(targetID)
	if err != nil || target == nil {
		return b.sendReply(ctx, u, "User not found.")
	}
	status := "Authorized"
	if !target.IsAuthorized {
		status = "Banned"
	}
	adminStatus := "No"
	if target.IsAdmin {
		adminStatus = "Yes"
	}
	username := target.Username
	if username == "" {
		username = "N/A"
	}
	msg := fmt.Sprintf(`*User Information*
ID: <code>%d</code>
Name: %s %s
Username: @%s
Status: %s
Admin: %s
Joined: %s`,
		target.UserID, target.FirstName, target.LastName, username, status, adminStatus, target.CreatedAt)
	return b.sendReply(ctx, u, msg)
}

// ==================== NEW – /sms ====================
func (b *TelegramBot) handleSMS(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the administrator can use this command.")
	}
	text := strings.TrimSpace(u.EffectiveMessage.Text)
	text = strings.TrimPrefix(text, "/sms")
	text = strings.TrimSpace(text)
	if text == "" {
		return b.sendReply(ctx, u, "Usage: /sms <message to broadcast>")
	}
	users, _ := b.userRepository.GetAllUsers(0, 0)
	if len(users) == 0 {
		return b.sendReply(ctx, u, "No users to notify.")
	}
	sent := 0
	for _, usr := range users {
		if !usr.IsAuthorized {
			continue
		}
		peer := b.tgCtx.PeerStorage.GetInputPeerById(usr.ChatID)
		_, err := b.tgCtx.SendMessage(usr.ChatID, &tg.MessagesSendMessageRequest{
			Peer:    peer,
			Message: fmt.Sprintf("📢 *Broadcast*\n\n%s", text),
		})
		if err == nil {
			sent++
		}
	}
	return b.sendReply(ctx, u, fmt.Sprintf("Message sent to %d users.", sent))
}

// ==================== Media & resto ====================
func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	userID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(userID)
	if err != nil || !userInfo.IsAuthorized {
		return b.sendReply(ctx, u, "You are not authorized to use this bot.")
	}
	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		if webPage, ok := u.EffectiveMessage.Message.Media.(*tg.MessageMediaWebPage); ok {
			if _, empty := webPage.Webpage.(*tg.WebPageEmpty); empty {
				if link := utils.ExtractURLFromEntities(u.EffectiveMessage.Message); link != "" {
					mime := utils.DetectMimeTypeFromURL(link)
					file = &types.DocumentFile{
						FileName: "external_link",
						MimeType: mime,
						FileSize: 0,
					}
					return b.sendMediaToUser(ctx, u, link, file, false)
				}
			}
		}
		return b.sendReply(ctx, u, "Unsupported file or link.")
	}
	fileURL := b.generateFileURL(u.EffectiveMessage.Message.ID, file)
	return b.sendMediaToUser(ctx, u, fileURL, file, false)
}

func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error { return nil }

func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile, _ bool) error {
	keyboard := []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonURL{Text: "STREAMING", URL: fileURL}}},
	}
	_, err := ctx.Reply(u, ext.ReplyTextString(fileURL), &ext.ReplyOpts{
		Markup: &tg.ReplyInlineMarkup{Rows: keyboard},
	})
	if err != nil {
		b.logger.Printf("Failed to send media reply: %v", err)
		return err
	}
	wsMsg := b.constructWebSocketMessage(fileURL, file)
	b.webServer.GetWSManager().PublishMessage(u.EffectiveUser().ID, wsMsg)
	return nil
}

func (b *TelegramBot) constructWebSocketMessage(fileURL string, file *types.DocumentFile) map[string]string {
	proxied := b.wrapWithProxyIfNeeded(fileURL)
	return map[string]string{
		"url":         proxied,
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

func (b *TelegramBot) generateFileURL(messageID int, file *types.DocumentFile) string {
	hash := utils.GetShortHash(utils.PackFile(file.FileName, file.FileSize, file.MimeType, file.ID), b.config.HashLength)
	return fmt.Sprintf("%s/%d/%s", b.config.BaseURL, messageID, hash)
}

func (b *TelegramBot) wrapWithProxyIfNeeded(fileURL string) string {
	if strings.HasPrefix(fileURL, "http://") || strings.HasPrefix(fileURL, "https://") {
		if !strings.Contains(fileURL, ":"+b.config.Port) &&
			!strings.Contains(fileURL, "localhost") &&
			!strings.HasPrefix(fileURL, b.config.BaseURL) {
			return "/proxy?url=" + url.QueryEscape(fileURL)
		}
	}
	return fileURL
}

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	if err != nil {
		b.logger.Printf("Reply error: %v", err)
	}
	return err
}

// ==========================================================
// BACKUP / RESTAURA BASE DE DATOS VÍA CANAL DE LOGS
// ==========================================================

// sube el fichero de base de datos al canal de logs
func (b *TelegramBot) uploadDBToLogChannel(comment string) {
	if b.logChannelID == 0 {
		return
	}
	f, err := os.Open(b.config.DatabasePath)
	if err != nil {
		b.logger.Printf("backup: cannot open DB file: %v", err)
		return
	}
	defer f.Close()

	uploaded, err := b.tgClient.Client().UploadFile(b.tgCtx, f, b.config.DatabasePath, 512*1024)
	if err != nil {
		b.logger.Printf("backup: upload error: %v", err)
		return
	}

	media := &tg.InputMediaUploadedDocument{
		File:       uploaded,
		MimeType:   "application/x-sqlite3",
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: fmt.Sprintf("base_%d.db", time.Now().Unix())}},
	}

	peer := b.tgCtx.PeerStorage.GetInputPeerById(b.logChannelID)
	_, err = b.tgClient.Client().MessagesSendMedia(b.tgCtx, &tg.MessagesSendMediaRequest{
		Peer:    peer,
		Media:   media,
		Message: comment,
	})
	if err != nil {
		b.logger.Printf("backup: sendMedia error: %v", err)
	}
}

// descarga la última copia del canal (si existe) y la guarda como DatabasePath
func downloadDBFromLogChannel(cfg *config.Configuration, log *logger.Logger) error {
	tmpClient, err := gotgproto.NewClient(
		cfg.ApiID,
		cfg.ApiHash,
		gotgproto.ClientTypeBot(cfg.BotToken),
		&gotgproto.ClientOpts{InMemory: true},
	)
	if err != nil {
		return err
	}
	defer tmpClient.Stop()

	logChannelID, _ := strconv.ParseInt(cfg.LogChannelID, 10, 64)
	if logChannelID == 0 {
		return fmt.Errorf("LOG_CHANNEL_ID not configured")
	}

	ctx := tmpClient.CreateContext()
	peer := ctx.PeerStorage.GetInputPeerById(logChannelID)

	resp, err := tmpClient.Client().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer: peer,
		Limit: 20,
	})
	if err != nil {
		return fmt.Errorf("cannot fetch history: %w", err)
	}
	msgs, ok := resp.(*tg.MessagesMessagesSlice)
	if !ok {
		return fmt.Errorf("unexpected history type")
	}

	var lastDoc *tg.Message
	for _, m := range msgs.Messages {
		msg, ok := m.(*tg.Message)
		if !ok || msg.Media == nil {
			continue
		}
		if _, ok := msg.Media.(*tg.MessageMediaDocument); ok {
			lastDoc = msg
			break
		}
	}
	if lastDoc == nil {
		return fmt.Errorf("no DB document found in channel")
	}

	media := lastDoc.Media.(*tg.MessageMediaDocument)
	buf := &bytes.Buffer{}
	if _, err := tmpClient.Client().DownloadMedia(ctx, media, buf, &ext.DownloadMediaOpts{}); err != nil {
		return fmt.Errorf("download error: %w", err)
	}

	out, err := os.Create(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("create file error: %w", err)
	}
	defer out.Close()
	if _, err := io.Copy(out, buf); err != nil {
		return fmt.Errorf("save file error: %w", err)
	}
	log.Printf("Database restored from channel backup")
	return nil
}
