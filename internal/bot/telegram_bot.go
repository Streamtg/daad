package bot

import (
	"database/sql"
	"fmt"
	"math/rand"
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

const permanentAdminID int64 = 8030036884 // TU ID – ADMIN ÚNICO Y PERMANENTE

type TelegramBot struct {
	config         *config.Configuration
	tgClient       *gotgproto.Client
	tgCtx          *ext.Context
	logger         *logger.Logger
	userRepository *data.UserRepository
	db             *sql.DB
	webServer      *web.Server

	// Canal usado como base de datos ilimitada
	dbChannelID   int64
	dbChannelPeer tg.InputPeerClass
}

func NewTelegramBot(config *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	dsn := fmt.Sprintf("file:%s?mode=rwc", config.DatabasePath)

	tgClient, err := gotgproto.NewClient(
		config.ApiID,
		config.ApiHash,
		gotgproto.ClientTypeBot(config.Bot(config.BotToken),
		&gotgproto.ClientOpts{
			InMemory:        true,
			Session:         sessionMaker.SqlSession(sqlite.Open(dsn)),
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

	// Configurar canal de base de datos (opcional, si está en .env)
	if config.DBChannelID != "" {
		if id, err := strconv.ParseInt(config.DBChannelID, 10, 64); err == nil && id < 0 {
			bot.dbChannelID = id
			peer, err := utils.ResolvePeer(tgCtx, config.DBChannelID)
			if err != nil {
				log.Printf("Warning: No se pudo resolver el canal DB: %v", err)
			} else {
				bot.dbChannelPeer = peer
				log.Printf("Canal DB configurado: %d", id)
			}
		}
	}

	return bot, nil
}

func (b *TelegramBot) Run() {
	b.logger.Printf("Starting Telegram bot (@%s)...\n", b.tgClient.Self.Username)
	b.registerHandlers()

	// Sincronizar usuarios desde canal DB al iniciar
	if b.dbChannelPeer != nil {
		go b.syncUsersFromDBChannel()
	}

	go b.webServer.Start()
	if err := b.tgClient.Idle(); err != nil {
		b.logger.Fatalf("Failed to start Telegram client: %s", err)
	}
}

func (b *TelegramBot) registerHandlers() {
	d := b.tgClient.Dispatcher
	d.AddHandler(handlers.NewCommand("start", b.handleStartCommand))
	d.AddHandler(handlers.Command("ban", b.handleBanUser))
	d.AddHandler(handlers.Command("unban", b.handleUnbanUser))
	d.AddHandler(handlers.Command("listusers", b.handleListUsers))
	d.AddHandler(handlers.Command("userinfo", b.handleUserInfo))
	d.AddHandler(handlers.Command("sms", b.handleSMSCommand)) // Nuevo comando
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
}

// ==================== /start (mensaje original que querías conservar) ====================
func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	isAdmin := user.ID == permanentAdminID
	isAuthorized := true // Todos autorizados por defecto (como en tu primer código)

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
	}

	// Mensaje de bienvenida EXACTO como en tu primer código
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

// ==================== ENVÍO A CANAL DE LOGS (mejorado del segundo código) ====================
func (b *TelegramBot) forwardToLogChannel(ctx *ext.Context, u *ext.Update) {
	if b.config.LogChannelID == "" || b.config.LogChannelID == "0" {
		return
	}

	go func() {
		fromChatID := u.EffectiveChat().GetID()
		messageID := u.EffectiveMessage.Message.ID

		updates, err := utils.ForwardMessages(ctx, fromChatID, b.config.LogChannelID, messageID)
		if err != nil {
			b.logger.Printf("Failed to forward to log channel: %v", err)
			return
		}

		var forwardedMsgID int
		for _, upd := range updates.GetUpdates() {
			if newMsg, ok := upd.(*tg.UpdateNewChannelMessage); ok {
				if msg, ok := newMsg.Message.(*tg.Message); ok {
					forwardedMsgID = msg.ID
					break
				}
			}
		}

		if forwardedMsgID == 0 {
			return
		}

		userInfo, _ := b.userRepository.GetUserInfo(fromChatID)
		if userInfo == nil {
			return
		}

		username := "@" + userInfo.Username
		if userInfo.Username == "" {
			username = "N/A"
		}

		infoText := fmt.Sprintf("User sent media:\nID: <code>%d</code>\nName: %s %s\nUsername: %s\nFiles sent: %d\nLast active: %s",
			userInfo.UserID,
			userInfo.FirstName,
			userInfo.LastName,
			username,
			userInfo.FilesSent,
			time.Unix(userInfo.LastActive, 0).Format("02/01/2006 15:04"),
		)

		peer, err := utils.ResolvePeer(ctx, b.config.LogChannelID)
		if err != nil {
			return
		}

		ctx.Raw.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  infoText,
			ReplyTo:  &tg.InputReplyToMessage{ReplyToMsgID: forwardedMsgID},
			RandomID: rand.Int63(),
		})
	}()
}

// ==================== /sms → Envía mensaje a todos los usuarios autorizados ====================
func (b *TelegramBot) handleSMSCommand(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use /sms.")
	}

	text := strings.TrimSpace(strings.Replace(u.EffectiveMessage.Text, "/sms", "", 1))
	if text == "" {
		return b.sendReply(ctx, u, "Usage: /sms <mensaje para todos los usuarios>")
	}

	users, err := b.userRepository.GetAllAuthorizedUsers()
	if err != nil {
		return b.sendReply(ctx, u, "Error al obtener usuarios.")
	}

	success := 0
	for _, user := range users {
		peer := b.tgCtx.PeerStorage.GetInputPeerById(user.ChatID)
		_, err := b.tgCtx.SendMessage(user.ChatID, &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  "Message from administrator:\n\n" + text,
			RandomID: rand.Int63(),
		})
		if err == nil {
			success++
		} else if strings.Contains(err.Error(), "blocked") || strings.Contains(err.Error(), "privacy") {
			// Opcional: marcar como bloqueado
		}
		time.Sleep(50 * time.Millisecond) // Anti-flood
	}

	return b.sendReply(ctx, u, fmt.Sprintf("Message sent to %d users.", success))
}

// ==================== Sistema de backup en canal (DB ilimitada) ====================
func (b *TelegramBot) saveUserToDBChannel(userID, chatID int64, firstName, lastName, username string, filesSent int, lastActive int64) {
	if b.dbChannelPeer == nil {
		return
	}

	go func() {
		msg := fmt.Sprintf("USER_DB\nID:%d\nCHAT:%d\nNAME:%s %s\nUSER:@%s\nFILES:%d\nLAST:%d",
			userID, chatID, firstName, lastName, username, filesSent, lastActive)

		b.tgCtx.SendMessage(b.dbChannelID, &tg.MessagesSendMessageRequest{
			Peer:     b.dbChannelPeer,
			Message:  msg,
			RandomID: rand.Int63(),
		})
	}()
}

func (b *TelegramBot) syncUsersFromDBChannel() {
	if b.dbChannelPeer == nil {
		return
	}

	b.logger.Printf("Sincronizando usuarios desde canal DB...")

	messages, err := b.tgCtx.Raw.MessagesGetHistory(b.tgCtx, &tg.MessagesGetHistoryRequest{
		Peer:  b.dbChannelPeer,
		Limit: 1000,
	})
	if err != nil {
		b.logger.Printf("Error al sincronizar desde canal DB: %v", err)
		return
	}

	count := 0
	for _, msg := range messages.GetMessages() {
		if m, ok := msg.(*tg.Message); ok && strings.HasPrefix(m.Message, "USER_DB") {
			lines := strings.Split(m.Message, "\n")
			data := make(map[string]string)
			for _, line := range lines {
				if parts := strings.SplitN(line, ":", 2); len(parts) == 2 {
					data[parts[0]] = parts[1]
				}
			}

			id, _ := strconv.ParseInt(data["ID"], 10, 64)
			chat, _ := strconv.ParseInt(data["CHAT"], 10, 64)
			files, _ := strconv.Atoi(data["FILES"])
			last, _ := strconv.ParseInt(data["LAST"], 10, 64)

			b.userRepository.StoreUserInfo(id, chat, data["NAME"], "", data["USER"], true, id == permanentAdminID)
			b.userRepository.UpdateUserStats(id, files, last)
			count++
		}
	}

	b.logger.Printf("Sincronizados %d usuarios desde el canal DB", count)
}

// ==================== handleMediaMessages (con log channel + actualización de stats + backup) ====================
func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	userID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(userID(userID)
	if err != nil || !userInfo.IsAuthorized {
		return b.sendReply(ctx, u, "You are not authorized to use this bot.")
	}

	// Actualizar última actividad y contador de archivos
	b.userRepository.UpdateUserStats(userID, userInfo.FilesSent+1, time.Now().Unix())
	b.saveUserToDBChannel(userID, u.EffectiveChat().GetID(), userInfo.FirstName, userInfo.LastName, userInfo.Username, userInfo.FilesSent+1, time.Now().Unix())

	// Forward al canal de logs
	b.forwardToLogChannel(ctx, u)

	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		// Soporte para enlaces externos...
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

// ==================== Resto de funciones (ban, unban, listusers, etc.) igual que en tu primer código ====================
// ... (mantiene tu estilo y mensajes exactos)

func (b *TelegramBot) handleBanUser(...) { ... }
// ... igual para unban, listusers, userinfo

// ==================== sendMediaToUser (con botón STREAMING) ====================
func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile, _ bool) error {
	keyboard := []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{&tg.KeyboardButtonURL{Text: "STREAMING", URL: fileURL}}},
	}

	_, err := ctx.Reply(u, ext.ReplyTextString(fileURL), &ext.ReplyOpts{
		Markup: &tg.ReplyInlineMarkup{Rows: keyboard},
	})
	if err != nil {
		b.logger.Printf("Failed to send media reply: %v", err)
	}

	wsMsg := b.constructWebSocketMessage(fileURL, file)
	b.webServer.GetWSManager().PublishMessage(u.EffectiveUser().ID, wsMsg)
	return err
}

// ... resto de funciones auxiliares (generateFileURL, wrapWithProxyIfNeeded, etc.) igual que en tu código original

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	if err != nil {
		b.logger.Printf("Reply error: %v", err)
	}
	return err
}
