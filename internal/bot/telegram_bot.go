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
	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/celestix/gotgproto/storage"
	"github.com/glebarez/sqlite"
	"github.com/gotd/td/tg"
)

// TelegramBot is the bot core.
type TelegramBot struct {
	config         *config.Configuration
	tgClient       *gotgproto.Client
	tgCtx          *ext.Context
	logger         *logger.Logger
	userRepository *data.UserRepository
	db             *sql.DB
	webServer      *web.Server
}

// Replace with your permanent admin
const permanentAdminID int64 = 8030036884

// NewTelegramBot builds the bot and initializes DB and web server.
func NewTelegramBot(cfg *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	// seed rand for RandomID
	rand.Seed(time.Now().UnixNano())

	dsn := fmt.Sprintf("file:%s?mode=rwc", cfg.DatabasePath)

	tgClient, err := gotgproto.NewClient(
		cfg.ApiID,
		cfg.ApiHash,
		gotgproto.ClientTypeBot(cfg.BotToken),
		&gotgproto.ClientOpts{
			InMemory:          true,
			Session:           sessionMaker.SqlSession(sqlite.Open(dsn)),
			DisableCopyright:  true,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Telegram client: %w", err)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite db: %w", err)
	}

	userRepo := data.NewUserRepository(db)
	if err := userRepo.InitDB(); err != nil {
		return nil, fmt.Errorf("failed init user repository: %w", err)
	}

	tgCtx := tgClient.CreateContext()
	webServer := web.NewServer(cfg, tgClient, tgCtx, log, userRepo)

	return &TelegramBot{
		config:         cfg,
		tgClient:       tgClient,
		tgCtx:          tgCtx,
		logger:         log,
		userRepository: userRepo,
		db:             db,
		webServer:      webServer,
	}, nil
}

// Run starts dispatcher, web server and attempts initial sync with log channel.
func (b *TelegramBot) Run() {
	b.logger.Printf("Starting Telegram bot (@%s)...", b.tgClient.Self.Username)

	// register handlers
	b.registerHandlers()

	// start web server
	go b.webServer.Start()

	// validate log channel and attempt initial sync (non-blocking)
	if b.config.LogChannelID != "" && b.config.LogChannelID != "0" {
		go func() {
			if err := b.syncUsersFromLogChannel(); err != nil {
				b.logger.Printf("Initial sync from log channel failed: %v", err)
			} else {
				b.logger.Printf("Initial sync from log channel completed")
			}
		}()
	} else {
		b.logger.Printf("No LogChannelID configured; skipping log-channel sync.")
	}

	// start client idle
	if err := b.tgClient.Idle(); err != nil {
		b.logger.Fatalf("Failed to idle tg client: %v", err)
	}
}

// registerHandlers registers the bot command and message handlers.
func (b *TelegramBot) registerHandlers() {
	d := b.tgClient.Dispatcher
	// Basic commands
	d.AddHandler(handlers.NewCommand("start", b.handleStartCommand))
	d.AddHandler(handlers.NewCommand("authorize", b.handleAuthorizeUser))
	d.AddHandler(handlers.NewCommand("deauthorize", b.handleDeauthorizeUser))
	d.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
	d.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))
	d.AddHandler(handlers.NewCommand("ban", b.handleBanUser))
	d.AddHandler(handlers.NewCommand("unban", b.handleUnbanUser))
	d.AddHandler(handlers.NewCommand("sms", b.handleSendSMS))
	// Generic update handlers
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
	// Media messages
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
}

// ========================= Commands & Handlers =========================

// handleStartCommand registers/updates the user and notifies admins when needed.
func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()
	user := u.EffectiveUser()

	// ignore bot itself
	if user.ID == ctx.Self.ID {
		return nil
	}

	// Check existing
	existing, err := b.userRepository.GetUserInfo(user.ID)
	if err != nil && err != sql.ErrNoRows {
		b.logger.Printf("Error checking existing user: %v", err)
	}

	isAdmin := user.ID == permanentAdminID
	isAuthorized := false
	if existing != nil {
		isAuthorized = existing.IsAuthorized
		isAdmin = existing.IsAdmin
	}

	// If new user, store locally and write to log channel
	if existing == nil {
		if err := b.userRepository.StoreUserInfo(user.ID, chatID, user.FirstName, user.LastName, user.Username, isAuthorized, isAdmin); err != nil {
			b.logger.Printf("Failed to store new user: %v", err)
			return err
		}
		// write user entry to log channel
		if err := b.appendUserToLogChannel(user.ID, chatID, user.FirstName, user.LastName, user.Username, isAuthorized, isAdmin); err != nil {
			b.logger.Printf("Warning: failed to append user to log channel: %v", err)
		}
		// notify admins about new user (background)
		go b.notifyAdminsAboutNewUser(user, chatID)
	}

	// welcome message
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

	if err := b.sendReply(ctx, u, welcome); err != nil {
		b.logger.Printf("Failed to send welcome: %v", err)
	}

	if !isAuthorized {
		return b.sendReply(ctx, u, "You are not authorized to use this bot yet. Please wait for an administrator to authorize you.")
	}
	return nil
}

// handleAuthorizeUser grants authorization (and optional admin) to a user.
// Usage: /authorize <user_id> [admin]
func (b *TelegramBot) handleAuthorizeUser(ctx *ext.Context, u *ext.Update) error {
	caller := u.EffectiveUser()
	callerInfo, err := b.userRepository.GetUserInfo(caller.ID)
	if err != nil || callerInfo == nil || !callerInfo.IsAdmin {
		return b.sendReply(ctx, u, "You are not authorized to perform this action.")
	}

	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /authorize <user_id> [admin]")
	}
	targetID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}
	asAdmin := len(args) > 2 && args[2] == "admin"

	if err := b.userRepository.AuthorizeUser(targetID, asAdmin); err != nil {
		b.logger.Printf("AuthorizeUser error: %v", err)
		return b.sendReply(ctx, u, "Failed to authorize user.")
	}

	// Update log channel by appending new record (so channel is authoritative)
	info, _ := b.userRepository.GetUserInfo(targetID)
	if info != nil {
		if err := b.appendUserToLogChannel(info.UserID, info.ChatID, info.FirstName, info.LastName, info.Username, info.IsAuthorized, info.IsAdmin); err != nil {
			b.logger.Printf("Failed to append authorization to log channel: %v", err)
		}
		// notify target
		peer := b.tgCtx.PeerStorage.GetInputPeerById(info.ChatID)
		_ = b.tgCtx.SendMessage(info.ChatID, &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  fmt.Sprintf("You have been authorized%s to use this bot.", func() string { if asAdmin { return " as admin" }; return "" }()),
			RandomID: rand.Int63(),
		})
	}

	return b.sendReply(ctx, u, fmt.Sprintf("User %d authorized.", targetID))
}

// handleDeauthorizeUser revokes a user's authorization.
// Usage: /deauthorize <user_id>
func (b *TelegramBot) handleDeauthorizeUser(ctx *ext.Context, u *ext.Update) error {
	caller := u.EffectiveUser()
	callerInfo, err := b.userRepository.GetUserInfo(caller.ID)
	if err != nil || callerInfo == nil || !callerInfo.IsAdmin {
		return b.sendReply(ctx, u, "You are not authorized to perform this action.")
	}

	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /deauthorize <user_id>")
	}
	targetID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}

	if err := b.userRepository.DeauthorizeUser(targetID); err != nil {
		b.logger.Printf("DeauthorizeUser error: %v", err)
		return b.sendReply(ctx, u, "Failed to deauthorize user.")
	}

	// append deauth to log channel
	info, _ := b.userRepository.GetUserInfo(targetID)
	if info != nil {
		if err := b.appendUserToLogChannel(info.UserID, info.ChatID, info.FirstName, info.LastName, info.Username, info.IsAuthorized, info.IsAdmin); err != nil {
			b.logger.Printf("Failed to append deauth to log channel: %v", err)
		}
		// notify user
		peer := b.tgCtx.PeerStorage.GetInputPeerById(info.ChatID)
		_ = b.tgCtx.SendMessage(info.ChatID, &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  "You have been deauthorized from using this bot.",
			RandomID: rand.Int63(),
		})
	}

	return b.sendReply(ctx, u, fmt.Sprintf("User %d deauthorized.", targetID))
}

// handleListUsers returns paginated list for admins.
func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	caller := u.EffectiveUser()
	callerInfo, err := b.userRepository.GetUserInfo(caller.ID)
	if err != nil || callerInfo == nil || !callerInfo.IsAdmin {
		return b.sendReply(ctx, u, "You are not authorized to perform this action.")
	}

	const pageSize = 10
	page := 1
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) > 1 {
		if p, err := strconv.Atoi(args[1]); err == nil && p > 0 {
			page = p
		}
	}

	total, err := b.userRepository.GetUserCount()
	if err != nil {
		b.logger.Printf("GetUserCount error: %v", err)
		return b.sendReply(ctx, u, "Error retrieving user count.")
	}

	offset := (page - 1) * pageSize
	users, err := b.userRepository.GetAllUsers(offset, pageSize)
	if err != nil {
		b.logger.Printf("GetAllUsers error: %v", err)
		return b.sendReply(ctx, u, "Error retrieving users.")
	}

	if len(users) == 0 {
		return b.sendReply(ctx, u, "No users found.")
	}

	var sb strings.Builder
	sb.WriteString("*User List*\n\n")
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
		sb.WriteString(fmt.Sprintf("%d. `%d` – %s %s (@%s) – %s%s\n",
			offset+i+1, usr.UserID, usr.FirstName, usr.LastName, username, status, adminTag))
	}
	totalPages := (total + pageSize - 1) / pageSize
	sb.WriteString(fmt.Sprintf("\nPage %d of %d (%d total users)", page, totalPages, total))

	_, err = ctx.Reply(u, ext.ReplyTextString(sb.String()), &ext.ReplyOpts{})
	return err
}

// handleUserInfo shows a user's info to admins.
func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	caller := u.EffectiveUser()
	callerInfo, err := b.userRepository.GetUserInfo(caller.ID)
	if err != nil || callerInfo == nil || !callerInfo.IsAdmin {
		return b.sendReply(ctx, u, "You are not authorized to perform this action.")
	}

	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /userinfo <user_id>")
	}

	targetID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}

	t, err := b.userRepository.GetUserInfo(targetID)
	if err != nil || t == nil {
		return b.sendReply(ctx, u, "User not found.")
	}

	status := "Authorized"
	if !t.IsAuthorized {
		status = "Banned"
	}
	adminStatus := "No"
	if t.IsAdmin {
		adminStatus = "Yes"
	}
	username := t.Username
	if username == "" {
		username = "N/A"
	}

	msg := fmt.Sprintf(`*User Information*
ID: <code>%d</code>
ChatID: %d
Name: %s %s
Username: @%s
Status: %s
Admin: %s
Joined: %s`,
		t.UserID, t.ChatID, t.FirstName, t.LastName, username, status, adminStatus, t.CreatedAt)

	return b.sendReply(ctx, u, msg)
}

// handleBanUser bans (deauthorizes) a user permanently (admin-only).
func (b *TelegramBot) handleBanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use this command.")
	}
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /ban <user_id> [reason]")
	}
	targetID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
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
	// append to log channel
	info, _ := b.userRepository.GetUserInfo(targetID)
	if info != nil {
		_ = b.appendUserToLogChannel(info.UserID, info.ChatID, info.FirstName, info.LastName, info.Username, info.IsAuthorized, info.IsAdmin)
	}
	b.logger.Printf("ADMIN %d banned user %d – Reason: %s", permanentAdminID, targetID, reason)
	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been banned. Reason: %s", targetID, reason))
}

// handleUnbanUser unbans (authorizes) a user.
func (b *TelegramBot) handleUnbanUser(ctx *ext.Context, u *ext.Update) error {
	if u.EffectiveUser().ID != permanentAdminID {
		return b.sendReply(ctx, u, "Only the main administrator can use this command.")
	}
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /unban <user_id>")
	}
	targetID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}
	if err := b.userRepository.AuthorizeUser(targetID, false); err != nil {
		return b.sendReply(ctx, u, "Failed to unban user.")
	}
	info, _ := b.userRepository.GetUserInfo(targetID)
	if info != nil {
		_ = b.appendUserToLogChannel(info.UserID, info.ChatID, info.FirstName, info.LastName, info.Username, info.IsAuthorized, info.IsAdmin)
	}
	b.logger.Printf("ADMIN %d unbanned user %d", permanentAdminID, targetID)
	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been unbanned.", targetID))
}

// handleSendSMS sends a message to all authorized users.
// Usage: /sms Your message here
func (b *TelegramBot) handleSendSMS(ctx *ext.Context, u *ext.Update) error {
	caller := u.EffectiveUser()
	callerInfo, err := b.userRepository.GetUserInfo(caller.ID)
	if err != nil || callerInfo == nil || !callerInfo.IsAdmin {
		return b.sendReply(ctx, u, "You are not authorized to perform this action.")
	}
	args := strings.SplitN(u.EffectiveMessage.Text, " ", 2)
	if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
		return b.sendReply(ctx, u, "Usage: /sms <message>")
	}
	message := args[1]

	users, err := b.userRepository.GetAllUsers(0, 1000000) // get all users
	if err != nil {
		b.logger.Printf("GetAllUsers error for /sms: %v", err)
		return b.sendReply(ctx, u, "Failed to read users.")
	}

	count := 0
	for _, usr := range users {
		if !usr.IsAuthorized {
			continue
		}
		peer := b.tgCtx.PeerStorage.GetInputPeerById(usr.ChatID)
		// if ChatID is zero, skip
		if usr.ChatID == 0 {
			continue
		}
		_, err := b.tgCtx.SendMessage(usr.ChatID, &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  message,
			RandomID: rand.Int63(),
		})
		if err != nil {
			b.logger.Printf("Failed to send /sms to %d: %v", usr.UserID, err)
			continue
		}
		count++
		// small pause to avoid flooding
		time.Sleep(50 * time.Millisecond)
	}

	return b.sendReply(ctx, u, fmt.Sprintf("Sent message to %d users.", count))
}

// ========================= Media handling & logs =========================

// handleAnyUpdate logs debug info for updates.
func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error {
	if b.config.DebugMode {
		b.logger.Debugf("Received update")
		// minimal debug to avoid heavy log spam
		if u.EffectiveMessage != nil {
			b.logger.Debugf("Message ID: %d, From: %v, Media: %T", u.EffectiveMessage.Message.ID, u.EffectiveUser(), u.EffectiveMessage.Message.Media)
		}
		if u.CallbackQuery != nil {
			b.logger.Debugf("Callback from %d: %s", u.CallbackQuery.UserID, string(u.CallbackQuery.Data))
		}
	}
	return nil
}

// handleMediaMessages processes media, forwards to log channel, and sends streaming link back.
func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()
	user := u.EffectiveUser()

	// verify user type is user
	if !b.isUserChat(ctx, chatID) {
		return dispatcher.EndGroups
	}

	// get user info from local DB
	userInfo, err := b.userRepository.GetUserInfo(user.ID)
	if err != nil {
		if err == sql.ErrNoRows {
			return b.sendReply(ctx, u, "You are not authorized to use this bot yet.")
		}
		b.logger.Printf("GetUserInfo error: %v", err)
		return b.sendReply(ctx, u, "Internal error retrieving user info.")
	}
	if !userInfo.IsAuthorized {
		return b.sendReply(ctx, u, "You are not authorized to use this bot yet.")
	}

	// Forward the message to log channel (if configured)
	if b.config.LogChannelID != "" && b.config.LogChannelID != "0" {
		go func() {
			fromChatID := u.EffectiveChat().GetID()
			messageID := u.EffectiveMessage.Message.ID

			updates, err := utils.ForwardMessages(ctx, fromChatID, b.config.LogChannelID, messageID)
			if err != nil {
				b.logger.Printf("Failed to forward message %d from chat %d to log channel %s: %v", messageID, fromChatID, b.config.LogChannelID, err)
				return
			}

			// try to find resulting channel message id from updates
			var newMsgID int
			for _, up := range updates.GetUpdates() {
				if newChannelMsg, ok := up.(*tg.UpdateNewChannelMessage); ok {
					if m, ok := newChannelMsg.Message.(*tg.Message); ok {
						newMsgID = m.GetID()
						break
					}
				}
			}
			if newMsgID == 0 {
				b.logger.Printf("Could not determine new forwarded message id for original %d", messageID)
				return
			}

			// send info reply to the forwarded message in log channel
			infoMsg := fmt.Sprintf("Media from user:\nID: %d\nName: %s %s\nUsername: %s",
				userInfo.UserID, userInfo.FirstName, userInfo.LastName, func() string {
					if userInfo.Username != "" {
						return "@" + userInfo.Username
					}
					return "N/A"
				}(),
			)

			logPeer, err := utils.GetLogChannelPeer(ctx, b.config.LogChannelID)
			if err != nil {
				b.logger.Printf("Failed to get log channel peer: %v", err)
				return
			}

			_, err = ctx.Raw.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer:     logPeer,
				Message:  infoMsg,
				ReplyTo:  &tg.InputReplyToMessage{ReplyToMsgID: newMsgID},
				RandomID: rand.Int63(),
			})
			if err != nil {
				b.logger.Printf("Failed to send user info to log channel: %v", err)
			}
		}()
	}

	// Obtain file info and generate URL
	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		// check for web page link fallback
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
		b.logger.Printf("Unsupported media or error extracting file: %v", err)
		return b.sendReply(ctx, u, "Unsupported file or link.")
	}

	fileURL := b.generateFileURL(u.EffectiveMessage.Message.ID, file)
	return b.sendMediaToUser(ctx, u, fileURL, file, false)
}

// sendMediaToUser replies with the streaming URL and publishes to websocket manager.
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

// ========================= Utilities =========================

// isUserChat checks if a chat ID belongs to a user.
func (b *TelegramBot) isUserChat(ctx *ext.Context, chatID int64) bool {
	peer := ctx.PeerStorage.GetPeerById(chatID)
	// storage.TypeUser constant indicates user peer type
	if peer.Type != int(storage.TypeUser) {
		b.logger.Printf("Chat %d is not a user-type peer", chatID)
		return false
	}
	return true
}

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	if err != nil {
		b.logger.Printf("Reply error: %v", err)
	}
	return err
}

// constructWebSocketMessage builds the payload for web socket publishing.
func (b *TelegramBot) constructWebSocketMessage(fileURL string, file *types.DocumentFile) map[string]string {
	return map[string]string{
		"url":         b.wrapWithProxyIfNeeded(fileURL),
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
		if !strings.Contains(fileURL, fmt.Sprintf(":%s", b.config.Port)) &&
			!strings.Contains(fileURL, "localhost") &&
			!strings.HasPrefix(fileURL, b.config.BaseURL) {
			return fmt.Sprintf("/proxy?url=%s", url.QueryEscape(fileURL))
		}
	}
	return fileURL
}

// ========================= Log-channel DB sync =========================

// appendUserToLogChannel appends a USER record to the log channel.
// Format: USER|<user_id>|<chat_id>|<first>|<last>|<username>|<authorized>|<admin>
func (b *TelegramBot) appendUserToLogChannel(userID int64, chatID int64, first, last, username string, authorized bool, admin bool) error {
	if b.config.LogChannelID == "" || b.config.LogChannelID == "0" {
		return fmt.Errorf("log channel not configured")
	}
	peer, err := utils.GetLogChannelPeer(b.tgCtx, b.config.LogChannelID)
	if err != nil {
		return fmt.Errorf("GetLogChannelPeer failed: %w", err)
	}

	rec := fmt.Sprintf("USER|%d|%d|%s|%s|%s|%t|%t", userID, chatID, escapePipes(first), escapePipes(last), escapePipes(username), authorized, admin)

	_, err = b.tgCtx.SendMessageToPeer(peer, &tg.MessagesSendMessageRequest{
		Peer:     peer,
		Message:  rec,
		RandomID: rand.Int63(),
	})
	if err != nil {
		// fallback to ctx.Raw to ensure RandomID
		_, rawErr := b.tgCtx.Raw.MessagesSendMessage(b.tgCtx, &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  rec,
			RandomID: rand.Int63(),
		})
		if rawErr != nil {
			return fmt.Errorf("failed to send user record to log channel: %v / %v", err, rawErr)
		}
	}
	return nil
}

// syncUsersFromLogChannel reads log-channel messages and builds DB entries from USER| lines.
func (b *TelegramBot) syncUsersFromLogChannel() error {
	if b.config.LogChannelID == "" || b.config.LogChannelID == "0" {
		return fmt.Errorf("log channel not configured")
	}
	peer, err := utils.GetLogChannelPeer(b.tgCtx, b.config.LogChannelID)
	if err != nil {
		return fmt.Errorf("GetLogChannelPeer failed: %w", err)
	}

	// We'll page history to collect USER| lines.
	const pageSize = 200
	var offsetID int32 = 0
	for {
		req := &tg.MessagesGetHistoryRequest{
			Peer:  peer,
			Limit: int32(pageSize),
			// OffsetID zero means latest backwards
		}
		hist, err := b.tgCtx.Raw.MessagesGetHistory(b.tgCtx, req)
		if err != nil {
			return fmt.Errorf("MessagesGetHistory failed: %w", err)
		}

		// hist can be different concrete types; handle common ones
		var messages []tg.MessageClass
		switch hh := hist.(type) {
		case *tg.MessagesMessages:
			messages = hh.Messages
		case *tg.MessagesChannelMessages:
			messages = hh.Messages
		default:
			// try to assert via interface methods (best-effort)
			b.logger.Printf("Unexpected history response type: %T", hist)
		}

		if len(messages) == 0 {
			break
		}

		for _, mc := range messages {
			if m, ok := mc.(*tg.Message); ok {
				// parse message text
				if m.Message != "" && strings.HasPrefix(m.Message, "USER|") {
					// parse record
					parts := strings.SplitN(m.Message, "|", 8)
					if len(parts) >= 8 {
						uid, _ := strconv.ParseInt(parts[1], 10, 64)
						cid, _ := strconv.ParseInt(parts[2], 10, 64)
						first := unescapePipes(parts[3])
						last := unescapePipes(parts[4])
						username := unescapePipes(parts[5])
						auth, _ := strconv.ParseBool(parts[6])
						admin, _ := strconv.ParseBool(parts[7])

						// store/ensure in local repository
						existing, err := b.userRepository.GetUserInfo(uid)
						if err == sql.ErrNoRows || existing == nil {
							if err := b.userRepository.StoreUserInfo(uid, cid, first, last, username, auth, admin); err != nil {
								b.logger.Printf("Failed to store user from log channel %d: %v", uid, err)
							}
						} else {
							// update if different
							_ = b.userRepository.UpdateUserInfo(uid, cid, first, last, username, auth, admin)
						}
					}
				}
				offsetID = m.GetID()
			}
		}

		// stop if less than pageSize
		if len(messages) < pageSize {
			break
		}
		// else continue to fetch older messages by OffsetID
		// set OffsetId to messages[len(messages)-1].GetID() - 1
		lastMsg := messages[len(messages)-1]
		if lm, ok := lastMsg.(*tg.Message); ok {
			req.OffsetID = lm.GetID()
		} else {
			break
		}
	}

	return nil
}

// escapePipes replaces '|' to prevent splitting issues.
func escapePipes(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}
func unescapePipes(s string) string {
	return strings.ReplaceAll(s, "\\|", "|")
}

// notifyAdminsAboutNewUser sends an inline-button notification to admins so they can authorize.
func (b *TelegramBot) notifyAdminsAboutNewUser(newUser *tg.User, newUsersChatID int64) {
	admins, err := b.userRepository.GetAllAdmins()
	if err != nil {
		b.logger.Printf("Failed to get admins: %v", err)
		return
	}

	var notifyText string
	if uname, ok := newUser.GetUsername(); ok {
		notifyText = fmt.Sprintf("A new user has joined: *@%s* (%s %s)\nID: `%d`\n\nUse buttons below to manage authorization.", uname, escapeMarkdownV2(newUser.FirstName), escapeMarkdownV2(newUser.LastName), newUser.ID)
	} else {
		notifyText = fmt.Sprintf("A new user has joined: %s %s\nID: `%d`\n\nUse buttons below to manage authorization.", escapeMarkdownV2(newUser.FirstName), escapeMarkdownV2(newUser.LastName), newUser.ID)
	}

	markup := &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{Text: "Authorize", Data: []byte(fmt.Sprintf("auth,%d,authorize", newUser.ID))},
					&tg.KeyboardButtonCallback{Text: "Decline", Data: []byte(fmt.Sprintf("auth,%d,decline", newUser.ID))},
				},
			},
		},
	}

	for _, a := range admins {
		if a.UserID == newUser.ID && a.UserID == newUsersChatID {
			continue
		}
		peer := b.tgCtx.PeerStorage.GetInputPeerById(a.ChatID)
		req := &tg.MessagesSendMessageRequest{
			Peer:        peer,
			Message:     notifyText,
			ReplyMarkup: markup,
			RandomID:    rand.Int63(),
		}
		if _, err := b.tgCtx.SendMessage(a.ChatID, req); err != nil {
			b.logger.Printf("Failed notify admin %d: %v", a.UserID, err)
		}
	}
}

// escapeMarkdownV2 escapes Telegram markdownv2 characters.
func escapeMarkdownV2(text string) string {
	r := strings.NewReplacer(
		"_", "\\_", "*", "\\*", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)",
		"~", "\\~", "`", "\\`", ">", "\\>", "#", "\\#", "+", "\\+", "-", "\\-",
		"=", "\\=", "|", "\\|", "{", "\\{", "}", "\\}", ".", "\\.", "!", "\\!",
	)
	return r.Replace(text)
}
