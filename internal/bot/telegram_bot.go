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

const permanentAdminID int64 = 8030036884 // TU ID – ADMIN ÚNICO

type TelegramBot struct {
	config         *config.Configuration
	tgClient       *gotgproto.Client
	tgCtx          *ext.Context
	logger         *logger.Logger
	userRepository *data.UserRepository
	db             *sql.DB
	webServer      *web.Server

	dbChannelID   int64
	dbChannelPeer tg.InputPeerClass
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

	// Configurar canal DB si está definido en .env
	if config.DBChannelID != "" {
		if id, err := strconv.ParseInt(config.DBChannelID, 10, 64); err == nil && id != 0 {
			bot.dbChannelID = id
			peer, err := utils.ResolvePeer(tgCtx, config.DBChannelID)
			if err != nil {
				log.Printf("Warning: No se pudo resolver canal DB (%s): %v", config.DBChannelID, err)
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

	if b.dbChannelPeer != nil {
		go b.syncUsersFromDBChannel()
	}

	go b.webServer.Start()

	if err := b.tgproto.Client.Idle(); err != nil {
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

// ==================== /start (exacto como querías) ====================
func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	user := u.EffectiveUser()
	if user.ID == ctx.Self.ID {
		return nil
	}

	isAdmin := user.ID == permanentAdminID
	isAuthorized := true

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

// ==================== Media → Log Channel + Stats + DB Channel Backup ====================
func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	userID := u.EffectiveUser().ID

	userInfo, err := b.userRepository.GetUserInfo(userID)
	if err != nil || !userInfo.IsAuthorized {
		return b.sendReply(ctx, u, "You are not authorized to use this bot.")
	}

	// Actualizar estadísticas
	b.userRepository.UpdateUserStats(userID, userInfo.FilesSent+1, time.Now().Unix())

	// Guardar en canal DB (backup ilimitado)
	b.saveUserToDBChannel(
		userID,
		u.EffectiveChat().GetID(),
		userInfo.FirstName,
		userInfo.LastName,
		userInfo.Username,
		userInfo.FilesSent+1,
		time.Now().Unix(),
	)

	// Enviar al canal de logs
	b.forwardToLogChannel(ctx, u)

	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		if link := utils.ExtractURLFromEntities(u.EffectiveMessage.Message); link != "" {
			mime := utils.DetectMimeTypeFromURL(link)
			file = &types.DocumentFile{
				FileName: "external_link",
				MimeType: mime,
				FileSize: 0,
			}
			return b.sendMediaToUser(ctx, u, link, file, false)
		}
		return b.sendReply(ctx, u, "Unsupported file or link.")
	}

	fileURL := b.generateFileURL(u.EffectiveMessage.Message.ID, file)
	return b.sendMediaToUser(ctx, u, fileURL, file, false)
}

// ==================== /sms – Mensaje masivo ====================
func (b *TelegramBot) handleSMSCommand(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use this command.")
	}

	text := strings.TrimSpace(strings.TrimPrefix(u.EffectiveMessage.Text, "/sms"))
	if text == "" || text == "/sms" {
		return b.sendReply(ctx, u, "Usage: /sms <message>")
	}

	users, err := b.userRepository.GetAllAuthorizedUsers()
	if err != nil {
	{
		return b.sendReply(ctx, u, "Error retrieving users.")
	}

	success := 0
	for _, usr := range users {
		peer := b.tgCtx.PeerStorage.GetInputPeerById(usr.ChatID)
		_, err := b.tgCtx.SendMessage(usr.ChatID, &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  "Administrator message:\n\n" + text,
			RandomID: rand.Int63(),
		})
		if err == nil {
			success++
		}
		time.Sleep(50 * time.Millisecond)
	}

	return b.sendReply(ctx, u, fmt.Sprintf("Message sent to %d users.", success))
	}
}

// ==================== Canal como DB ilimitada ====================
func (b *TelegramBot) saveUserToDBChannel(userID, chatID int64, firstName, lastName, username string, filesSent int, lastActive int64) {
	if b.dbChannelPeer == nil {
		return
	}

	msg := fmt.Sprintf("USER_DB\nID:%d\nCHAT:%d\nNAME:%s %s\nUSER:%s\nFILES:%d\nLAST:%d",
		userID, chatID, firstName, lastName, username, filesSent, lastActive)

	go b.tgCtx.SendMessage(b.dbChannelID, &tg.MessagesSendMessageRequest{
		Peer:     b.dbChannelPeer,
		Message:  msg,
		RandomID: rand.Int63(),
	})
}

func (b *TelegramBot) syncUsersFromDBChannel() {
	if b.dbChannelPeer == nil {
		return
	}

	b.logger.Printf("Synchronizing users from DB channel...")

	history, err := b.tgCtx.Raw.MessagesGetHistory(b.tgCtx, &tg.MessagesGetHistoryRequest{
		Peer:  b.dbChannelPeer,
		Limit: 1000,
	})
	if err != nil {
		b.logger.Printf("Error syncing DB channel: %v", err)
		return
	}

	count := 0
	for _, m := range history.GetMessages() {
		if msg, ok := m.(*tg.Message); ok && strings.HasPrefix(msg.Message, "USER_DB") {
			lines := strings.Split(msg.Message, "\n")
			data := map[string]string{}
			for _, l := range lines[1:] {
				if parts := strings.SplitN(l, ":", 2); len(parts) == 2 {
					data[parts[0]] = parts[1]
				}
			}

			id, _ := strconv.ParseInt(data["ID"], 10, 64)
			chat, _ := strconv.ParseInt(data["CHAT"], 10, 64)
			files, _ := strconv.Atoi(data["FILES"])
			last, _ := strconv.ParseInt(data["LAST"], 10, 64)

			b.userRepository.StoreUserInfo(id, chat, strings.SplitN(data["NAME"], " ", 2)[0], "", data["USER"], true, id == permanentAdminID)
			b.userRepository.UpdateUserStats(id, files, last)
			count++
		}
	}
	b.logger.Printf("Synchronized %d users from DB channel", count)
}

// ==================== Log Channel Forward (del segundo código, mejorado) ====================
func (b *TelegramBot) forwardToLogChannel(ctx *ext.Context, u *ext.Update) {
	if b.config.LogChannelID == "" || b.config.LogChannelID == "0" {
		return
	}

	go func() {
		fromChatID := u.EffectiveChat().GetID()
		msgID := u.EffectiveMessage.Message.ID

		updates, err := utils.ForwardMessages(ctx, fromChatID, b.config.LogChannelID, msgID)
		if err != nil {
			b.logger.Printf("Failed to forward to log channel: %v", err)
			return
		}

		var forwardedID int
		for _, upd := range updates.GetUpdates() {
			if newMsg, ok := upd.(*tg.UpdateNewChannelMessage); ok {
				if m, ok := newMsg.Message.(*tg.Message); ok {
					forwardedID = m.ID
					break
				}
			}
		}

		if forwardedID == 0 {
			return
		}

		userInfo, _ := b.userRepository.GetUserInfo(fromChatID)
		if userInfo == nil {
			return
		}

		username := userInfo.Username
		if username == "" {
			username = "N/A"
		} else {
			username = "@" + username
		}

		infoMsg := fmt.Sprintf("Media received\nUser ID: <code>%d</code>\nName: %s %s\nUsername: %s\nFiles sent: %d\nLast active: %s",
			userInfo.UserID, userInfo.FirstName, userInfo.LastName, username, userInfo.FilesSent,
			time.Unix(userInfo.LastActive, 0).Format("02/01/2006 15:04"))

		peer, _ := utils.ResolvePeer(ctx, b.config.LogChannelID)
		ctx.Raw.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  infoMsg,
			ReplyTo: &tg.InputReplyToMessage{ReplyToMsgID: forwardedID},
			RandomID: rand.Int63(),
		})
	}()
}

// ==================== Resto de comandos (ban, unban, listusers, etc.) – igual que tu primer código ====================

func (b *TelegramBot) handleBanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use this command.")
	}
	// ... (igual que tu código original)
	// (lo dejo igual para no alargar, pero puedes copiarlo tal cual del primer código)
	return nil
}

// ... igual para unban, listusers, userinfo

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
		"url":       proxied,
		"fileName":  file.FileName,
		"fileId":    strconv.FormatInt(file.ID, 10),
		"mimeType":  file.MimeType,
		"duration":  strconv.Itoa(file.Duration),
		"width":    strconv.Itoa(file.Width),
		"height":   strconv.Itoa(file.Height),
		"title":     file.Title,
		"performer": file.Performer,
		"isVoice":   strconv.FormatBool(file.IsVoice),
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

func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error { return nil }
