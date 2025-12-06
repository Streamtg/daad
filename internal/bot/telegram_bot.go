// internal/bot/telegram_bot.go
package bot

import (
	"database/sql"
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"strings"

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

const permanentAdminID int64 = 8030036884

type TelegramBot struct {
	config         *config.Configuration
	tgClient       *gotgproto.Client
	tgCtx          *ext.Context
	logger         *logger.Logger
	userRepository *data.UserRepository
	db             *sql.DB
	webServer      *web.Server

	logChannelPeer tg.InputPeerClass
	logChannelID   int64
}

func NewTelegramBot(config *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	dsn := fmt.Sprintf("file:%s?mode=rwc", config.DatabasePath)

	tgClient, err := gotgproto.NewClient(
		config.ApiID,
		config.ApiHash,
		gotgproto.ClientTypeBot(config.BotToken),
		&gotgproto.ClientOpts{
			InMemory:         true,
			Session:          sessionMaker.SqlSession(sqlite.Open(dsn)),
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

	bot := &TelegramBot{
		config:         config,
		tgClient:       tgClient,
		tgCtx:          tgCtx,
		logger:         log,
		userRepository: userRepository,
		db:             db,
		webServer:      webServer,
	}

	// Configurar canal de logs + backup
	if config.LogChannelID != "" && config.LogChannelID != "0" {
		if id, err := strconv.ParseInt(config.LogChannelID, 10, 64); err == nil && id < 0 {
			bot.logChannelID = id
			bot.logChannelPeer = bot.tgCtx.PeerStorage.GetInputPeerById(id)
			log.Printf("Canal de logs y backup activado: %s", config.LogChannelID)
		}
	}

	return bot, nil
}

func (b *TelegramBot) Run() {
	b.logger.Printf("Starting Telegram bot (@%s...\n", b.tgClient.Self.Username)
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
	d.AddHandler(handlers.NewCommand("sms", b.handleSMSCommand))
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
}

// ==================== /start ====================
func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	isAdmin := user.ID == permanentAdminID
	isAuthorized := true

	_ = b.userRepository.StoreUserInfo(
		user.ID,
		u.EffectiveChat().GetID(),
		user.FirstName,
		user.LastName,
		user.Username,
		isAuthorized,
		isAdmin,
	)

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

// ==================== handleMediaMessages – REENVÍA + INFO + BACKUP ====================
func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	userID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(userID)
	if err != nil || !userInfo.IsAuthorized {
		return b.sendReply(ctx, u, "You are not authorized to use this bot.")
	}

	// Reenviar archivo + info al canal
	if b.logChannelPeer != nil {
		go b.forwardMediaToLogChannel(ctx, u, userInfo)
	}

	// Backup del usuario
	if b.logChannelPeer != nil {
		go b.saveUserBackup(userInfo)
	}

	// Procesar archivo
	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		if link := utils.ExtractURLFromEntities(u.EffectiveMessage.Message); link != "" {
			mime := utils.DetectMimeTypeFromURL(link)
			file = &types.DocumentFile{FileName: "external_link", MimeType: mime}
			return b.sendMediaToUser(ctx, u, link, file, false)
		}
		return b.sendReply(ctx, u, "Unsupported file or link.")
	}

	fileURL := b.generateFileURL(u.EffectiveMessage.Message.ID, file)
	return b.sendMediaToUser(ctx, u, fileURL, file, false)
}

// ==================== Reenvío al canal + info del usuario ====================
func (b *TelegramBot) forwardMediaToLogChannel(ctx *ext.Context, u *ext.Update, user *data.User) {
	fromChatID := u.EffectiveChat().GetID()
	msgID := u.EffectiveMessage.Message.ID

	updates, err := ctx.ForwardMessages(b.logChannelID, []int{msgID}, false, fromChatID)
	if err != nil {
		b.logger.Printf("Error forwarding media: %v", err)
		return
	}

	// Esperamos un poco para que el mensaje llegue y luego respondemos
	go func() {
		time.Sleep(1 * time.Second)
		username := user.Username
		if username == "" {
			username = "N/A"
		} else {
			username = "@" + username
		}

		info := fmt.Sprintf("Archivo recibido\nUsuario: %s %s\nUsername: %s\nID: <code>%d</code>",
			user.FirstName, user.LastName, username, user.UserID)

		b.tgCtx.SendMessage(b.logChannelID, &tg.MessagesSendMessageRequest{
			Peer:     b.logChannelPeer,
			Message:  info,
			RandomID: rand.Int63(),
		})
	}()
}

// ==================== Backup del usuario en el canal ====================
func (b *TelegramBot) saveUserBackup(user *data.User) {
	msg := fmt.Sprintf("USER_BACKUP\nID:%d\nNAME:%s %s\nUSER:%s",
		user.UserID, user.FirstName, user.LastName, user.Username)

	b.tgCtx.SendMessage(b.logChannelID, &tg.MessagesSendMessageRequest{
		Peer:     b.logChannelPeer,
		Message:  msg,
		RandomID: rand.Int63(),
	})
}

// ==================== Botón STREAMING ====================
func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile, _ bool) error {
	keyboard := []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonURL{Text: "STREAMING", URL: fileURL}}},
	}

	_, err := ctx.Reply(u, ext.ReplyTextString(fileURL), &ext.ReplyOpts{
		Markup: &tg.ReplyInlineMarkup{Rows: keyboard},
	})
	if err != nil {
		b.logger.Printf("Failed to send reply: %v", err)
	}

	wsMsg := b.constructWebSocketMessage(fileURL, file)
	b.webServer.GetWSManager().PublishMessage(u.EffectiveUser().ID, wsMsg)
	return err
}

// ==================== Resto de funciones (sin cambios) ====================
func (b *TelegramBot) generateFileURL(messageID int, file *types.DocumentFile) string {
	hash := utils.GetShortHash(utils.PackFile(file.FileName, file.FileSize, file.MimeType, file.ID), b.config.HashLength)
	return fmt.Sprintf("%s/%d/%s", b.config.BaseURL, messageID, hash)
}

func (b *TelegramBot) wrapWithProxyIfNeeded(fileURL string) string {
	if strings.HasPrefix(fileURL, "http://") || strings.HasPrefix(fileURL, "https://") {
		if !strings.Contains(fileURL, ":"+b.config.Port) && !strings.Contains(fileURL, "localhost") && !strings.HasPrefix(fileURL, b.config.BaseURL) {
			return "/proxy?url=" + url.QueryEscape(fileURL)
		}
	}
	return fileURL
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

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	if err != nil {
		b.logger.Printf("Reply error: %v", err)
	}
	return err
}

func (b *TelegramBot) handleAnyUpdate(*ext.Context, *ext.Update) error { return nil }

// Comandos admin (ban, unban, listusers, etc.) los tienes ya perfectos en tu código original
// Puedes copiarlos tal cual de tu versión anterior
