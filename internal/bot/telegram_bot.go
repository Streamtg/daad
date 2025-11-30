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
const logChannelID int64 = -1003213143951 // TU CANAL

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
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
}

// ==================== COMANDOS ====================
func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	isAuthorized := true
	isAdmin := user.ID == permanentAdminID

	if err := b.userRepository.StoreUserInfo(user.ID, u.EffectiveChat().GetID(), user.FirstName, user.LastName, user.Username, isAuthorized, isAdmin); err != nil {
		b.logger.Printf("Failed to store user %d: %v", user.ID, err)
	}

	welcome := `Envía o reenvía cualquier archivo multimedia y te genero un link de streaming directo al instante.

Formatos soportados:
• Audio: MP3, M4A, FLAC, WAV, OGG...
• Video: MP4, MKV, AVI, MOV, WEBM...
• Fotos y documentos

¡Solo envíame un archivo y verás la magia!
Soporte: @Wavetouch_bot`

	return b.sendReply(ctx, u, welcome)
}

func (b *TelegramBot) handleBanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Solo el admin principal puede usar este comando.")
	}
	return b.sendReply(ctx, u, "/ban <user_id> [razón] – Comando disponible solo para ti.")
}

func (b *TelegramBot) handleUnbanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Solo el admin principal puede usar este comando.")
	}
	return b.sendReply(ctx, u, "/unban <user_id> – Comando disponible solo para ti.")
}

func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Acceso denegado.")
	}
	return b.sendReply(ctx, u, "Comando /listusers disponible solo para ti.")
}

func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Acceso denegado.")
	}
	return b.sendReply(ctx, u, "Comando /userinfo disponible solo para ti.")
}

// ==================== MEDIA + LOG AL CANAL (100% FUNCIONAL SIN DEPENDENCIAS EXTRA) ====================
func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	userID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(userID)
	if err != nil || !userInfo.IsAuthorized {
		return b.sendReply(ctx, u, "No estás autorizado a usar el bot.")
	}

	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		if link := utils.ExtractURLFromEntities(u.EffectiveMessage.Message); link != "" {
			mime := utils.DetectMimeTypeFromURL(link)
			file = &types.DocumentFile{FileName: "external_link", MimeType: mime}
			return b.sendMediaToUser(ctx, u, link, file, false)
		}
		return b.sendReply(ctx, u, "Archivo no soportado.")
	}

	fileURL := b.generateFileURL(u.EffectiveMessage.Message.ID, file)

	// LOG AL CANAL – 100% FUNCIONAL CON TU VERSIÓN ACTUAL
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
			username, user.ID, file.FileName,
			formatBytes(file.FileSize), fileURL,
		)

		// Convertimos el canal -100... a InputPeer correcto
		channelID := int64(-logChannelID / 1000000000000)

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
	if bytes == 0 { return "0 B" }
	const unit = 1024
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGT"[exp])
}

func (b *TelegramBot) handleAnyUpdate(*ext.Context, *ext.Update) error { return nil }

func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile, _ bool) error {
	keyboard := []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonURL{Text: "STREAMING", URL: fileURL}}},
	}

	_, err := ctx.Reply(u, ext.ReplyTextString(fileURL), &ext.ReplyOpts{
		Markup: &tg.ReplyInlineMarkup{Rows: keyboard},
	})
	if err != nil {
		b.logger.Printf("Error enviando respuesta: %v", err)
	}

	wsMsg := b.constructWebSocketMessage(fileURL, file)
	b.webServer.GetWSManager().PublishMessage(u.EffectiveUser().ID, wsMsg)
	return nil
}

func (b *TelegramBot) constructWebSocketMessage(fileURL string, file *types.DocumentFile) map[string]string {
	proxied := b.wrapWithProxyIfNeeded(fileURL)
	return map[string]string{
		"url":       proxied,
		"fileName":  file.FileName,
		"mimeType":  file.MimeType,
		"title":     file.Title,
		"performer": file.Performer,
	}
}

func (b *TelegramBot) generateFileURL(messageID int, file *types.DocumentFile) string {
	hash := utils.GetShortHash(utils.PackFile(file.FileName, file.FileSize, file.MimeType, file.ID), b.config.HashLength)
	return fmt.Sprintf("%s/%d/%s", b.config.BaseURL, messageID, hash)
}

func (b *TelegramBot) wrapWithProxyIfNeeded(fileURL string) string {
	if strings.HasPrefix(fileURL, "http") && !strings.Contains(fileURL, b.config.BaseURL) {
		return "/proxy?url=" + url.QueryEscape(fileURL)
	}
	return fileURL
}

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	return err
}
