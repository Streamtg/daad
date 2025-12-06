// internal/bot/telegram_bot.go
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

const permanentAdminID int64 = 8030036884 // TU ID

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

	// Soporte para canal como DB ilimitada
	if config.DBChannelID != "" {
		if id, err := strconv.ParseInt(config.DBChannelID, 10, 64); err == nil && id != 0 {
			bot.dbChannelID = id
			peer, err := tgCtx.ResolvePeer(config.DBChannelID)
			if err != nil {
				log.Printf("Warning: No se pudo resolver canal DB: %v", err)
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

// ==================== /start – EXACTO como lo querías ====================
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

// ==================== /ban ====================
func (b *TelegramBot) handleBanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use this command.")
	}
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /ban <user_id> [reason]")
	}
	targetID, _ := strconv.ParseInt(args[1], 10, 64)
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
	b.logger.Printf("ADMIN %d banned user %d – %s", permanentAdminID, targetID, reason)
	go b.notifyUser(targetID, fmt.Sprintf("You have been permanently banned.\nReason: %s", reason))
	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been banned.\nReason: %s", targetID, reason))
}

// ==================== /unban ====================
func (b *TelegramBot) handleUnbanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use this command.")
	}
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /unban <user_id>")
	}
	targetID, _ := strconv.ParseInt(args[1], 10, 64)
	if err := b.userRepository.AuthorizeUser(targetID, false); err != nil {
		return b.sendReply(ctx, u, "Failed to unban user.")
	}
	go b.notifyUser(targetID, "You have been unbanned! You can use the bot again.")
	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been unbanned.", targetID))
}

// ==================== /listusers ====================
func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use this command.")
	}
	const pageSize = 10
	page := 1
	if len(strings.Fields(u.EffectiveMessage.Text)) > 1 {
		if p, _ := strconv.Atoi(strings.Fields(u.EffectiveMessage.Text)[1]); p > 0 {
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
		username := usr.Username
		if username == "" {
			username = "N/A"
		}
		msg.WriteString(fmt.Sprintf("%d. `%d` – %s %s (@%s) – %s\n",
			offset+i+1, usr.UserID, usr.FirstName, usr.LastName, username, status))
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
	targetID, _ := strconv.ParseInt(args[1], 10, 64)
	target, err := b.userRepository.GetUserInfo(targetID)
	if err != nil || target == nil {
		return b.sendReply(ctx, u, "User not found.")
	}
	status := "Authorized"
	if !target.IsAuthorized {
		status = "Banned"
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
Files sent: %d
Last active: %s`,
		target.UserID, target.FirstName, target.LastName, username, status,
		target.FilesSent, time.Unix(target.LastActive, 0).Format("02/01/2006 15:04"))
	return b.sendReply(ctx, u, msg)
}

// ==================== /sms – Mensaje masivo ====================
func (b *TelegramBot) handleSMSCommand(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use /sms.")
	}
	text := strings.TrimSpace(strings.TrimPrefix(u.EffectiveMessage.Text, "/sms"))
	if text == "" {
		return b.sendReply(ctx, u, "Usage: /sms <mensaje>")
	}
	users, err := b.userRepository.GetAllAuthorizedUsers()
	if err != nil {
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
		time.Sleep(33 * time.Millisecond)
	}
	return b.sendReply(ctx, u, fmt.Sprintf("Message sent to %d users.", success))
}

// ==================== Media + Logs + Stats + DB Channel ====================
func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	userID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(userID)
	if err != nil || !userInfo.IsAuthorized {
		return b.sendReply(ctx, u, "You are not authorized to use this bot.")
	}

	// Actualizar estadísticas (aseguramos que existan los campos)
	b.userRepository.UpdateUserStats(userID, userInfo.FilesSent+1, time.Now().Unix())

	// Backup en canal DB
	b.saveUserToDBChannel(userID, u.EffectiveChat().GetID(), userInfo.FirstName,
		userInfo.LastName, userInfo.Username, userInfo.FilesSent+1, time.Now().Unix())

	// Forward a canal de logs
	b.forwardToLogChannel(ctx, u)

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

// ... (resto de funciones: sendMediaToUser, generateFileURL, wrapWithProxyIfNeeded, etc.)
// → están al final del archivo, exactamente como en tu código original

// ==================== Utilidades ====================
func (b *TelegramBot) notifyUser(userID int64, text string) {
	info, _ := b.userRepository.GetUserInfo(userID)
	if info == nil || info.ChatID == 0 {
		return
	}
	peer := b.tgCtx.PeerStorage.GetInputPeerById(info.ChatID)
	b.tgCtx.SendMessage(info.ChatID, &tg.MessagesSendMessageRequest{
		Peer:     peer,
		Message:  text,
		RandomID: rand.Int63(),
	})
}

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

func (b *TelegramBot) syncUsersFromDBChannel() { /* ... igual que antes ... */ }

func (b *TelegramBot) forwardToLogChannel(ctx *ext.Context, u *ext.Update) { /* ... igual que antes ... */ }

func (b *TelegramBot) sendMediaToUser(...) error { /* igual que tu código original */ }

func (b *TelegramBot) generateFileURL(...) string { /* igual */ }

func (b *TelegramBot) wrapWithProxyIfNeeded(...) string { /* igual */ }

func (b *TelegramBot) constructWebSocketMessage(...) map[string]string { /* igual */ }

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	return err
}

func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error { return nil }
