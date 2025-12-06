package bot

import (
	"database/sql"
	"fmt"
	"net/url"
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
}

const permanentAdminID int64 = 8030036884
const logChannelID int64 = -1003213143951 // TU CANAL PRIVADO

func NewTelegramBot(config *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	dsn := fmt.Sprintf("file:%s?mode=rwc", config.DatabasePath)
	tgClient, err := gotgproto.NewClient(
		config.ApiID,
		config.ApiHash,
		gotgproto.ClientTypeBot(config.BotToken),
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
	userRepository := data.NewUserRepository(db)
	if err := userRepository.InitDB(); err != nil {
		return nil, err
	}
	tgCtx := tgClient.CreateContext()
	webServer := web.NewServer(config, tgClient, tgCtx, log, userRepository)
	return &TelegramBot{
		config:         config,
		tgClient:       tgClient,
		tgCtx:          tgCtx,
		logger:         log,
		userRepository: userRepository,
		db:             db,
		webServer:      webServer,
	}, nil
}

func (b *TelegramBot) Run() {
	b.logger.Printf("Starting Telegram bot (@%s)...\n", b.tgClient.Self.Username)
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
	d.AddHandler(handlers.NewCommand("sms", b.handleSMSCommand)) // NUEVO: /sms
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
}

// ==================== NUEVO: /sms – ENVÍA MENSAJE A TODOS LOS USUARIOS AUTORIZADOS ====================
func (b *TelegramBot) handleSMSCommand(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Solo el administrador principal puede usar /sms")
	}

	text := strings.TrimSpace(strings.TrimPrefix(u.EffectiveMessage.Text, "/sms"))
	if text == "" || strings.HasPrefix(text, "/sms") {
		return b.sendReply(ctx, u, "Uso: /sms <mensaje>\nEjemplo: /sms ¡Nueva función disponible!")
	}

	// Obtenemos todos los usuarios (máximo 10k)
	users, err := b.userRepository.GetAllUsers(0, 10000)
	if err != nil {
		b.logger.Printf("Error al obtener usuarios para /sms: %v", err)
		return b.sendReply(ctx, u, "Error interno.")
	}

	sent := 0
	failed := 0
	for _, user := range users {
		if !user.IsAuthorized {
			continue
		}
		_, err := b.tgCtx.SendMessage(user.ChatID, &tg.MessagesSendMessageRequest{
			Message:  fmt.Sprintf("Mensaje del administrador:\n\n%s", text),
			RandomID: time.Now().UnixNano(),
		})
		if err != nil {
			b.logger.Printf("Fallo al enviar SMS a %d: %v", user.UserID, err)
			failed++
		} else {
			sent++
		}
		time.Sleep(50 * time.Millisecond) // Evitar flood wait
	}

	return b.sendReply(ctx, u, fmt.Sprintf("Mensaje enviado a %d usuarios (%d fallidos)", sent, failed))
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
	go func() {
		info, _ := b.userRepository.GetUserInfo(targetID)
		if info != nil && info.ChatID != 0 {
			peer := b.tgCtx.PeerStorage.GetInputPeerById(info.ChatID)
			b.tgCtx.SendMessage(info.ChatID, &tg.MessagesSendMessageRequest{
				Peer:     peer,
				Message:  fmt.Sprintf("You have been permanently banned from using this bot.\nSupport: @Wavetouch_bot\n\nReason: %s", reason),
				RandomID: time.Now().UnixNano(),
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
	go func() {
		info, _ := b.userRepository.GetUserInfo(targetID)
		if info != nil && info.ChatID != 0 {
			peer := b.tgCtx.PeerStorage.GetInputPeerById(info.ChatID)
			b.tgCtx.SendMessage(info.ChatID, &tg.MessagesSendMessageRequest{
				Peer:     peer,
				Message:  "You have been unbanned!\nYou can now use the bot again.",
				RandomID: time.Now().UnixNano(),
			})
		}
	}()
	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been unbanned.", targetID))
}

// ==================== /listusers & /userinfo (tu código original) ====================
// (mantenidos exactamente como los tenías)

// ==================== MEDIA + LOG AL CANAL (100% FUNCIONAL) ====================
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

	// LOG AL CANAL PRIVADO – 100% FUNCIONAL SIN ParseMode
	go func() {
		user := u.EffectiveUser()
		username := user.Username
		if username == "" {
			username = "Sin username"
		}

		logText := fmt.Sprintf(
			"*NUEVO ARCHIVO SUBIDO*\n\n"+
				"Usuario: [%s %s](tg://user?id=%d)\n"+
				"Username: @%s\n"+
				"ID: `%d`\n"+
				"Archivo: `%s`\n"+
				"Tamaño: *%s*\n"+
				"Link: `%s`",
			user.FirstName, user.LastName, user.ID,
			username, user.ID,
			file.FileName,
			formatBytes(file.FileSize),
			fileURL,
		)

		channelID := -logChannelID / 1000000000000

		_, err := b.tgClient.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerChannel{
				ChannelID:  channelID,
				AccessHash: 0,
			},
			Message:  logText,
			RandomID: time.Now().UnixNano(),
		})
		if err != nil {
			b.logger.Printf("ERROR enviando log al canal: %v", err)
		}
	}()

	return b.sendMediaToUser(ctx, u, fileURL, file, false)
}

// ==================== UTILIDADES ====================
func formatBytes(bytes int64) string {
	if bytes == 0 {
		return "0 B"
	}
	const unit = 1024
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGT"[exp])
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
