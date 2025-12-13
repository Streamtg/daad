package bot

import (
	"database/sql"
	"fmt"
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
	"github.com/gotd/td/tg"
	"github.com/joho/godotenv"
)

type TelegramBot struct {
	config         *config.Configuration
	tgClient       *gotgproto.Client
	tgCtx          *ext.Context
	logger         *logger.Logger
	userRepository *data.UserRepository
	db             *sql.DB
	webServer      *web.Server
}

const permanentAdminID int64 = 8030036884

var (
	logChannelID int64
	botInstance  *TelegramBot
)

func NewTelegramBot(cfg *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	_ = godotenv.Load() // Carga .env automáticamente

	var err error
	logChannelID, _ = strconv.ParseInt(os.Getenv("LOG_CHANNEL_ID"), 10, 64)

	// Conexión a Supabase PostgreSQL
	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=require",
		cfg.SupabaseHost,
		cfg.SupabasePort,
		cfg.SupabaseUser,
		cfg.SupabasePassword,
		cfg.SupabaseDatabase,
	)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open Supabase database: %w", err)
	}

	// Pool de conexiones optimizado para producción
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(time.Hour)

	if err = db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping Supabase: %w", err)
	}
	log.Println("Connected to Supabase PostgreSQL successfully")

	// Cliente Telegram (sesión en memoria – ideal para bots)
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

	userRepo := data.NewUserRepository(db)
	if err := userRepo.InitDB(); err != nil {
		return nil, err
	}

	ctx := client.CreateContext()
	webSrv := web.NewServer(cfg, client, ctx, log, userRepo)

	b := &TelegramBot{
		config:         cfg,
		tgClient:       client,
		tgCtx:          ctx,
		logger:         log,
		userRepository: userRepo,
		db:             db,
		webServer:      webSrv,
	}

	botInstance = b
	return b, nil
}

func (b *TelegramBot) Run() {
	b.logger.Printf("Bot started @%s\n", b.tgClient.Self.Username)
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
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
}

// ==================== COMANDOS ====================

func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	isAdmin := user.ID == permanentAdminID

	_ = b.userRepository.StoreUserInfo(
		user.ID,
		u.EffectiveChat().GetID(),
		user.FirstName,
		user.LastName,
		user.Username,
		true,
		isAdmin,
	)

	logToChannel(fmt.Sprintf("New user: %s %s (@%s) - ID: %d",
		user.FirstName, user.LastName, user.Username, user.ID))

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

func (b *TelegramBot) handleSMSCommand(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use this command.")
	}

	message := strings.TrimSpace(strings.TrimPrefix(u.EffectiveMessage.Text, "/sms"))
	if message == "" {
		return b.sendReply(ctx, u, "Usage: /sms <your message>")
	}

	// Obtener todos los usuarios autorizados desde Supabase
	users, err := b.userRepository.GetAllUsers(0, 50000) // límite alto para broadcast
	if err != nil {
		return b.sendReply(ctx, u, "Error loading users from database.")
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
			time.Sleep(33 * time.Millisecond) // Rate limit seguro
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

	_ = b.userRepository.DeauthorizeUser(targetID)
	b.sendReply(ctx, u, fmt.Sprintf("User %d has been banned.\nReason: %s", targetID, reason))
	logToChannel(fmt.Sprintf("Admin banned user %d – Reason: %s", targetID, reason))
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

	_ = b.userRepository.AuthorizeUser(targetID, false)
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

	total, _ := b.userRepository.GetUserCount()
	offset := (page - 1) * pageSize
	users, _ := b.userRepository.GetAllUsers(offset, pageSize)

	var sb strings.Builder
	sb.WriteString("*User List*\n\n")
	for i, usr := range users {
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

	info, err := b.userRepository.GetUserInfo(targetID)
	if err != nil || info == nil {
		return b.sendReply(ctx, u, "User not found.")
	}

	status := "Authorized"
	if !info.IsAuthorized {
		status = "Banned"
	}
	admin := "No"
	if info.IsAdmin {
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
	info, err := b.userRepository.GetUserInfo(userID)
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

func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile) error {
	proxied := b.wrapWithProxyIfNeeded(fileURL)

	keyboard := tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonURL{Text: "STREAMING", URL: proxied}}},
		},
	}

	_, err := ctx.Reply(u, ext.ReplyTextString(proxied), &ext.ReplyOpts{Markup: &keyboard})
	if err != nil {
		b.logger.Printf("Failed to send streaming link: %v", err)
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
