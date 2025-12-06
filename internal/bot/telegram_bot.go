package bot

import (
	"database/sql"
	"encoding/json"
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

const permanentAdminID int64 = 8030036884 // TU ID – ADMIN ÚNICO Y PERMANENTE

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

	bot := &TelegramBot{
		config:         config,
		tgClient:       tgClient,
		tgCtx:          tgCtx,
		logger:         log,
		userRepository: userRepository,
		db:             db,
		webServer:      webServer,
	}

	// Iniciar sincronización desde canal de logs en background
	go func() {
		// Espera breve para que client esté listo
		time.Sleep(1500 * time.Millisecond)
		if err := bot.SyncUsersFromLogChannel(); err != nil {
			bot.logger.Printf("Warning: failed initial sync from log channel: %v", err)
		} else {
			bot.logger.Printf("Initial DB sync from log channel completed.")
		}
	}()

	return bot, nil
}

func (b *TelegramBot) Run() {
	b.logger.Printf("Starting Telegram bot (@%s)...\n", b.tgClient.Self.Username)
	b.registerHandlers()
	go b.webServer.Start()
	// Start a periodic resync in background to ensure DB stays up-to-date across instances
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			if err := b.SyncUsersFromLogChannel(); err != nil {
				b.logger.Printf("Periodic sync error: %v", err)
			}
		}
	}()
	if err := b.tgClient.Idle(); err != nil {
		b.logger.Fatalf("Failed to start Telegram client: %s", err)
	}
}

func (b *TelegramBot) registerHandlers() {
	d := b.tgClient.Dispatcher
	d.AddHandler(handlers.NewCommand("start", b.handleStartCommand))
	d.AddHandler(handlers.NewCommand("sms", b.handleSMSCommand)) // NUEVO
	d.AddHandler(handlers.NewCommand("ban", b.handleBanUser))
	d.AddHandler(handlers.NewCommand("unban", b.handleUnbanUser))
	d.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
	d.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))
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

	// Publicar registro estructurado en canal de logs para persistencia remota (DB)
	if err := b.publishUserRecordToLogChannel(user, u.EffectiveChat().GetID(), isAuthorized, isAdmin); err != nil {
		// no fatal: solo logueamos
		b.logger.Printf("Failed to publish user record to log channel: %v", err)
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
				Peer:    peer,
				Message: fmt.Sprintf("You have been permanently banned from using this bot.\nSupport: @Wavetouch_bot\n\nReason: %s", reason),
			})
		}
	}()

	// Also publish deauth to log channel (keeps remote DB state readable)
	if err := b.publishAdminActionToLogChannel(fmt.Sprintf("BAN|%d|%s", targetID, reason)); err != nil {
		b.logger.Printf("Failed to publish ban action to log channel: %v", err)
	}

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
				Peer:    peer,
				Message: "You have been unbanned!\nYou can now use the bot again.",
			})
		}
	}()

	// Publish unban action to log channel
	if err := b.publishAdminActionToLogChannel(fmt.Sprintf("UNBAN|%d", targetID)); err != nil {
		b.logger.Printf("Failed to publish unban action to log channel: %v", err)
	}

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

// ==================== Media & resto ====================
func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	userID := u.EffectiveUser().ID

	// Ensure user exists in repo (create if missing)
	userInfo, _ := b.userRepository.GetUserInfo(userID)
	if userInfo == nil {
		isAuthorized := true
		isAdmin := userID == permanentAdminID
		_ = b.userRepository.StoreUserInfo(
			userID,
			u.EffectiveChat().GetID(),
			u.EffectiveUser().FirstName,
			u.EffectiveUser().LastName,
			u.EffectiveUser().Username,
			isAuthorized,
			isAdmin,
		)
		// Publish to log channel so remote DB gets it
		_ = b.publishUserRecordToLogChannel(u.EffectiveUser(), u.EffectiveChat().GetID(), isAuthorized, isAdmin)
	}

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

	// 1) Log structured record to channel (USER|...)
	_ = b.publishUserRecordToLogChannel(u.EffectiveUser(), u.EffectiveChat().GetID(), true, userID == permanentAdminID)

	// 2) Publish readable meta to logs
	b.logIncomingFile(u, file)

	// 3) Forward original message to logs (keep file in channel)
	if b.config.LogChannelID != 0 {
		fromPeer := b.tgCtx.PeerStorage.GetInputPeerById(u.EffectiveChat().GetID())
		toPeer := b.tgCtx.PeerStorage.GetInputPeerById(b.config.LogChannelID)
		if fromPeer != nil && toPeer != nil {
			_, _ = b.tgCtx.API().MessagesForwardMessages(&tg.MessagesForwardMessagesRequest{
				FromPeer: fromPeer,
				ID:       []int{u.EffectiveMessage.Message.ID},
				ToPeer:   toPeer,
			})
		}
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

// ==================== NUEVAS FUNCIONES: LOG & SYNC ====================

// publishUserRecordToLogChannel escribe una línea estructurada que actúa como fuente de verdad
// Formato (single line): USER|<user_id>|<chat_id>|<first>|<last>|<username>|<authorized>|<isAdmin>
func (b *TelegramBot) publishUserRecordToLogChannel(user *tg.User, chatID int64, authorized bool, isAdmin bool) error {
	if b.config.LogChannelID == 0 {
		return fmt.Errorf("log channel not configured")
	}
	username := user.Username
	if username == "" {
		username = "N/A"
	}
	line := fmt.Sprintf("USER|%d|%d|%s|%s|%s|%t|%t", user.ID, chatID, escapePipe(user.FirstName), escapePipe(user.LastName), escapePipe(username), authorized, isAdmin)
	// Publish structured line
	peer := b.tgCtx.PeerStorage.GetInputPeerById(b.config.LogChannelID)
	if peer == nil {
		return fmt.Errorf("failed to get input peer for log channel")
	}
	_, err := b.tgCtx.SendMessage(b.config.LogChannelID, &tg.MessagesSendMessageRequest{
		Peer:    peer,
		Message: line,
	})
	if err != nil {
		return err
	}
	// Also publish a human readable meta message for convenience (non-structured)
	meta := fmt.Sprintf("📁 NEW USER\nID: %d\nName: %s %s\nUsername: @%s\nTime: %s", user.ID, user.FirstName, user.LastName, user.Username, time.Now().Format(time.RFC3339))
	_, _ = b.tgCtx.SendMessage(b.config.LogChannelID, &tg.MessagesSendMessageRequest{
		Peer:    peer,
		Message: meta,
	})
	return nil
}

// publishAdminActionToLogChannel publica acciones administrativas (BAN/UNBAN)
func (b *TelegramBot) publishAdminActionToLogChannel(line string) error {
	if b.config.LogChannelID == 0 {
		return fmt.Errorf("log channel not configured")
	}
	peer := b.tgCtx.PeerStorage.GetInputPeerById(b.config.LogChannelID)
	if peer == nil {
		return fmt.Errorf("failed to get input peer for log channel")
	}
	_, err := b.tgCtx.SendMessage(b.config.LogChannelID, &tg.MessagesSendMessageRequest{
		Peer:    peer,
		Message: line,
	})
	return err
}

// logIncomingFile publica meta legible al canal de logs
func (b *TelegramBot) logIncomingFile(u *ext.Update, file *types.DocumentFile) {
	if b.config.LogChannelID == 0 {
		return
	}
	user := u.EffectiveUser()
	text := fmt.Sprintf(
		"📁 NEW FILE UPLOADED\nUser: %d\nName: %s %s\nUsername: @%s\nFile: %s\nMime: %s\nSize: %d\nMessageID: %d\nChatID: %d\nTime: %s",
		user.ID,
		user.FirstName,
		user.LastName,
		user.Username,
		file.FileName,
		file.MimeType,
		file.FileSize,
		u.EffectiveMessage.Message.ID,
		u.EffectiveChat().GetID(),
		time.Now().Format(time.RFC3339),
	)
	peer := b.tgCtx.PeerStorage.GetInputPeerById(b.config.LogChannelID)
	if peer == nil {
		return
	}
	_, _ = b.tgCtx.SendMessage(b.config.LogChannelID, &tg.MessagesSendMessageRequest{
		Peer:    peer,
		Message: text,
	})
}

// SyncUsersFromLogChannel lee el historial del canal de logs y reconstruye la base de datos
func (b *TelegramBot) SyncUsersFromLogChannel() error {
	if b.config.LogChannelID == 0 {
		return fmt.Errorf("log channel not configured")
	}

	peer := b.tgCtx.PeerStorage.GetInputPeerById(b.config.LogChannelID)
	if peer == nil {
		return fmt.Errorf("unable to obtain peer for log channel")
	}

	// Lectura por batches para no saturar
	limit := int(1000)
	var offsetID int = 0
	for {
		history, err := b.tgCtx.API().MessagesGetHistory(&tg.MessagesGetHistoryRequest{
			Peer:  peer,
			Limit: int(limit),
			// OffsetDate, OffsetID, MaxID etc. se dejan 0 para leer secuencialmente
			OffsetID: offsetID,
		})
		if err != nil {
			return fmt.Errorf("error getting history: %w", err)
		}

		msgs, ok := history.(*tg.MessagesMessages)
		if !ok {
			// si no se puede castear, terminamos
			break
		}

		if len(msgs.Messages) == 0 {
			break
		}

		maxID := 0
		for _, m := range msgs.Messages {
			msg, ok := m.(*tg.Message)
			if !ok || msg.Message == "" {
				continue
			}
			// Mantener max id para siguiente offset
			if msg.ID > maxID {
				maxID = msg.ID
			}
			// Procesar líneas con USER|
			if strings.HasPrefix(msg.Message, "USER|") {
				b.processUserLine(msg.Message)
			}
			// Opcionalmente procesar BAN/UNBAN acciones si las publicaste
			if strings.HasPrefix(msg.Message, "BAN|") {
				// BAN|<userID>|<reason>
				parts := strings.SplitN(msg.Message, "|", 3)
				if len(parts) >= 2 {
					if id, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
						_ = b.userRepository.DeauthorizeUser(id)
					}
				}
			}
			if strings.HasPrefix(msg.Message, "UNBAN|") {
				parts := strings.SplitN(msg.Message, "|", 2)
				if len(parts) >= 2 {
					if id, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
						_ = b.userRepository.AuthorizeUser(id, false)
					}
				}
			}
		}

		// Si no hubo mensajes nuevos, rompemos
		if maxID == 0 {
			break
		}
		// Avanzar offsetID para evitar leer infinitamente; +1 para next batch
		offsetID = maxID + 1
		// Si la cantidad de mensajes leídos es menor al límite, terminamos
		if len(msgs.Messages) < limit {
			break
		}
	}

	return nil
}

// processUserLine parsea una línea USER|... y la guarda en userRepository
func (b *TelegramBot) processUserLine(line string) {
	// USER|<user_id>|<chat_id>|<first>|<last>|<username>|<authorized>|<isAdmin>
	parts := strings.Split(line, "|")
	if len(parts) < 8 {
		return
	}
	uid, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return
	}
	chatID, _ := strconv.ParseInt(parts[2], 10, 64)
	first := unescapePipe(parts[3])
	last := unescapePipe(parts[4])
	username := unescapePipe(parts[5])
	authorized := parts[6] == "true"
	isAdmin := parts[7] == "true"

	// Store or update in repository
	_ = b.userRepository.StoreUserInfo(uid, chatID, first, last, username, authorized, isAdmin)
}

// escapePipe para sanitizar pipes en nombres
func escapePipe(s string) string {
	return strings.ReplaceAll(s, "|", " ")
}
func unescapePipe(s string) string {
	return strings.ReplaceAll(s, "|", " ")
}

// ==================== /sms (ENVÍO GLOBAL) ====================
func (b *TelegramBot) handleSMSCommand(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use this command.")
	}

	args := strings.TrimSpace(strings.TrimPrefix(u.EffectiveMessage.Text, "/sms"))
	if args == "" {
		return b.sendReply(ctx, u, "Usage: /sms <message text here>")
	}

	// Obtener todos los usuarios -- usamos GetAllUsers con límite alto
	const big = 1000000
	users, err := b.userRepository.GetAllUsers(0, big)
	if err != nil {
		return b.sendReply(ctx, u, "Failed to fetch users for broadcast.")
	}

	sent := 0
	for _, usr := range users {
		if !usr.IsAuthorized {
			continue
		}
		peer := b.tgCtx.PeerStorage.GetInputPeerById(usr.ChatID)
		if peer == nil {
			continue
		}
		_, err := b.tgCtx.SendMessage(usr.ChatID, &tg.MessagesSendMessageRequest{
			Peer:    peer,
			Message: args,
		})
		if err == nil {
			sent++
		}
		// small delay to avoid flood
		time.Sleep(40 * time.Millisecond)
	}

	// registrar acción en logs (opcional)
	_ = b.publishAdminActionToLogChannel(fmt.Sprintf("SMS|%d|%s", sent, args))

	return b.sendReply(ctx, u, fmt.Sprintf("SMS sent to %d users.", sent))
}
