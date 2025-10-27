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

const (
	callbackResendToPlayer = "cb Raoul_ResendToPlayer"
	// Callbacks eliminados: Play, Restart, Fwd10, Bwd10, ToggleFullscreen, ListUsers, UserAuthAction
)

// TelegramBot represents the main bot structure.
type TelegramBot struct {
	config         *config.Configuration
	tgClient       *gotgproto.Client
	tgCtx          *ext.Context
	logger         *logger.Logger
	userRepository *data.UserRepository
	db             *sql.DB
	webServer      *web.Server
}

// NewTelegramBot creates a new instance of TelegramBot.
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
		})
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

// Run starts the Telegram bot and web server.
func (b *TelegramBot) Run() {
	b.logger.Printf("Starting Telegram bot (@%s)...\n", b.tgClient.Self.Username)
	b.registerHandlers()
	go b.webServer.Start()
	if err := b.tgClient.Idle(); err != nil {
		b.logger.Fatalf("Failed to start Telegram client: %s", err)
	}
}

func (b *TelegramBot) registerHandlers() {
	clientDispatcher := b.tgClient.Dispatcher
	clientDispatcher.AddHandler(handlers.NewCommand("start", b.handleStartCommand))
	clientDispatcher.AddHandler(handlers.NewCommand("authorize", b.handleAuthorizeUser))
	clientDispatcher.AddHandler(handlers NewCommand("deauthorize", b.handleDeauthorizeUser))
	clientDispatcher.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
	clientDispatcher.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))
	// Eliminado: handler de callbacks (ya no se usan botones)
	clientDispatcher.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
	clientDispatcher.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
}

func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()
	user := u.EffectiveUser()

	if user.ID == ctx.Self.ID {
		b.logger.Printf("Ignoring /start command from bot's own ID (%d).", user.ID)
		return nil
	}

	b.logger.Printf("Received /start command from user: %s (ID: %d) in chat: %d", user.FirstName, user.ID, chatID)

	existingUser, err := b.userRepository.GetUserInfo(user.ID)
	if err != nil && err != sql.ErrNoRows {
		b.logger.Printf("Failed to retrieve user info from DB for /start: %v", err)
		return fmt.Errorf("failed to retrieve user info for start command: %w", err)
	}

	isFirstUser, err := b.userRepository.IsFirstUser()
	if err != nil {
		b.logger.Printf("Failed to check if user is first: %v", err)
		return fmt.Errorf("failed to check first user status: %w", err)
	}

	isAdmin := false
	isAuthorized := false

	if existingUser == nil {
		if isFirstUser {
			isAuthorized = true
			isAdmin = true
			b.logger.Printf("User %d is the first user and has been automatically granted admin rights.", user.ID)
		}

		err = b.userRepository.StoreUserInfo(user.ID, chatID, user.FirstName, user.LastName, user.Username, isAuthorized, isAdmin)
		if err != nil {
			b.logger.Printf("Failed to store user info for new user %d: %v", user.ID, err)
			return fmt.Errorf("failed to store user info: %w", err)
		}
		b.logger.Printf("Stored new user %d with isAuthorized=%t, isAdmin=%t", user.ID, isAuthorized, isAdmin)

		if !isAdmin {
			go b.notifyAdminsAboutNewUser(user, chatID)
		}
	} else {
		isAuthorized = existingUser.IsAuthorized
		isAdmin = existingUser.IsAdmin
	}

	webURL := fmt.Sprintf("%s/%d", b.config.BaseURL, chatID)
	startMsg := fmt.Sprintf(
		"Hello %s, I am @%s, your bridge between Telegram and the Web!\n\n"+
			"Send or forward media files (audio, video, photos, or documents) to this bot.\n"+
			"I will instantly generate a streaming link and play it on your web player.\n\n"+
			"Features:\n"+
			"• Forward media from any chat\n"+
			"• Upload media directly (including video files as documents)\n"+
			"• Instant web streaming\n\n"+
			"Your player: %s",
		user.FirstName, ctx.Self.Username, webURL,
	)

	err = b.sendMediaURLReply(ctx, u, startMsg, webURL)
	if err != nil {
		b.logger.Printf("Failed to send start message to user %d: %v", user.ID, err)
		return fmt.Errorf("failed to send start message: %w", err)
	}

	if !isAuthorized {
		authorizationMsg := "You are not authorized to use this bot yet. Please ask one of the administrators to authorize you and wait until you receive a confirmation."
		return b.sendReply(ctx, u, authorizationMsg)
	}
	return nil
}

func (b *TelegramBot) notifyAdminsAboutNewUser(newUser *tg.User, newUsersChatID int64) {
	admins, err := b.userRepository.GetAllAdmins()
	if err != nil {
		b.logger.Printf("Failed to retrieve admin list: %v", err)
		return
	}

	var notificationMsg string
	username, hasUsername := newUser.GetUsername()
	if hasUsername {
		notificationMsg = fmt.Sprintf("A new user has joined: *@%s* (%s %s)\nID: `%d`\n\n_Use the buttons below to manage authorization\\._", username, escapeMarkdownV2(newUser.FirstName), escapeMarkdownV2(newUser.LastName), newUser.ID)
	} else {
		notificationMsg = fmt.Sprintf("A new user has joined: %s %s\nID: `%d`\n\n_Use the buttons below to manage authorization\\._", escapeMarkdownV2(newUser.FirstName), escapeMarkdownV2(newUser.LastName), newUser.ID)
	}

	markup := &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{Text: "Authorize", Data: []byte(fmt.Sprintf("%s,%d,authorize", "cb_user_auth_action", newUser.ID))},
					&tg.KeyboardButtonCallback{Text: "Decline", Data: []byte(fmt.Sprintf("%s,%d,decline", "cb_user_auth_action", newUser.ID))},
				},
			},
		},
	}

	for _, admin := range admins {
		if admin.UserID == newUser.ID {
			continue
		}
		b.logger.Printf("Notifying admin %d about new user %d", admin.UserID, newUser.ID)

		peer := b.tgCtx.PeerStorage.GetInputPeerById(admin.ChatID)
		req := &tg.MessagesSendMessageRequest{
			Peer:        peer,
			Message:     notificationMsg,
			ReplyMarkup: markup,
		}
		_, err = b.tgCtx.SendMessage(admin.ChatID, req)
		if err != nil {
			b.logger.Printf("Failed to notify admin %d: %v", admin.UserID, err)
		}
	}
}

func (b *TelegramBot) handleAuthorizeUser(ctx *ext.Context, u *ext.Update) error {
	b.logger.Printf("Received /authorize command from user ID: %d", u.EffectiveUser().ID)

	adminID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(adminID)
	if err != nil || !userInfo.IsAdmin {
		return b.sendReply(ctx, u, "You are not authorized to perform this action.")
	}

	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /authorize <user_id> [admin]")
	}
	targetUserID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}

	isAdmin := len(args) > 2 && args[2] == "admin"
	err = b.userRepository.AuthorizeUser(targetUserID, isAdmin)
	if err != nil {
		b.logger.Printf("Failed to authorize user %d: %v", targetUserID, err)
		return b.sendReply(ctx, u, "Failed to authorize the user.")
	}

	adminMsgSuffix := ""
	if isAdmin {
		adminMsgSuffix = " as an admin"
	}

	targetUserInfo, err := b.userRepository.GetUserInfo(targetUserID)
	if err == nil {
		peer := b.tgCtx.PeerStorage.GetInputPeerById(targetUserInfo.ChatID)
		req := &tg.MessagesSendMessageRequest{
			Peer:    peer,
			Message: fmt.Sprintf("You have been authorized%s to use WebBridgeBot!", adminMsgSuffix),
		}
		_, err = b.tgCtx.SendMessage(targetUserInfo.ChatID, req)
		if err != nil {
			b.logger.Printf("Could not send notification to authorized user %d: %v", targetUserID, err)
		}
	}

	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been authorized%s.", targetUserID, adminMsgSuffix))
}

func (b *TelegramBot) handleDeauthorizeUser(ctx *ext.Context, u *ext.Update) error {
	b.logger.Printf("Received /deauthorize command from user ID: %d", u.EffectiveUser().ID)

	adminID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(adminID)
	if err != nil || !userInfo.IsAdmin {
		return b.sendReply(ctx, u, "You are not authorized to perform this action.")
	}

	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /deauthorize <user_id>")
	}
	targetUserID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}

	err = b.userRepository.DeauthorizeUser(targetUserID)
	if err != nil {
		b.logger.Printf("Failed to deauthorize user %d: %v", targetUserID, err)
		return b.sendReply(ctx, u, "Failed to deauthorize the user.")
	}

	targetUserInfo, err := b.userRepository.GetUserInfo(targetUserID)
	if err == nil {
		peer := b.tgCtx.PeerStorage.GetInputPeerById(targetUserInfo.ChatID)
		req := &tg.MessagesSendMessageRequest{
			Peer:    peer,
			Message: "You have been deauthorized from using WebBridgeBot.",
		}
		_, err = b.tgCtx.SendMessage(targetUserInfo.ChatID, req)
		if err != nil {
			b.logger.Printf("Could not send notification to deauthorized user %d: %v", targetUserID, err)
		}
	}

	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been deauthorized.", targetUserID))
}

func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error {
	if b.config.DebugMode {
		b.logger.Debugf("Received update from user")
		if u.EffectiveMessage != nil {
			user := u.EffectiveUser()
			chatID := u.EffectiveChat().GetID()
			message := u.EffectiveMessage

			b.logger.Debugf("Message from user: %s %s (ID: %d, Username: @%s) in chat: %d",
				user.FirstName, user.LastName, user.ID, user.Username, chatID)

			if message.Message.Media != nil {
				b.logger.Debugf("Media attached - Type: %T", message.Message.Media)
			}
		}
	}
	return nil
}

func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()
	user := u.EffectiveUser()

	isForwarded := u.EffectiveMessage.Message.GetFwdFrom() != nil
	messageType := "direct upload"
	if isForwarded {
		messageType = "forwarded message"
	}

	b.logger.Printf("Received media %s from user: %s (ID: %d) in chat: %d", messageType, user.FirstName, user.ID, chatID)

	if !b.isUserChat(ctx, chatID) {
		return dispatcher.EndGroups
	}

	existingUser, err := b.userRepository.GetUserInfo(chatID)
	if err != nil {
		if err == sql.ErrNoRows {
			return b.sendReply(ctx, u, "You are not authorized to use this bot yet. Please ask one of the administrators to authorize you.")
		}
		return fmt.Errorf("failed to retrieve user info: %w", err)
	}

	if !existingUser.IsAuthorized {
		return b.sendReply(ctx, u, "You are not authorized to use this bot yet. Please ask one of the administrators to authorize you.")
	}

	// Log channel forwarding (unchanged)
	if b.config.LogChannelID != "" && b.config.LogChannelID != "0" {
		go func() {
			fromChatID := u.EffectiveChat().GetID()
			messageID := u.EffectiveMessage.Message.ID
			updates, err := utils.ForwardMessages(ctx, fromChatID, b.config.LogChannelID, messageID)
			if err != nil {
				b.logger.Printf("Failed to forward message to log channel: %v", err)
				return
			}
			var newMsgID int
			for _, update := range updates.GetUpdates() {
				if newMsg, ok := update.(*tg.UpdateNewChannelMessage); ok {
					if m, ok := newMsg.Message.(*tg.Message); ok {
						newMsgID = m.GetID()
						break
					}
				}
			}
			if newMsgID == 0 {
				return
			}
			userInfo, _ := b.userRepository.GetUserInfo(fromChatID)
			infoMsg := fmt.Sprintf("Media from user:\nID: %d\nName: %s %s\nUsername: @%s",
				userInfo.UserID, userInfo.FirstName, userInfo.LastName, userInfo.Username)
			logChannelPeer, _ := utils.GetLogChannelPeer(ctx, b.config.LogChannelID)
			_, _ = ctx.Raw.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer:     logChannelPeer,
				Message:  infoMsg,
				ReplyTo:  &tg.InputReplyToMessage{ReplyToMsgID: newMsgID},
				RandomID: rand.Int63(),
			})
		}()
	}

	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		if webPageMedia, ok := u.EffectiveMessage.Message.Media.(*tg.MessageMediaWebPage); ok {
			if _, isEmpty := webPageMedia.Webpage.(*tg.WebPageEmpty); isEmpty {
				fileURL := utils.ExtractURLFromEntities(u.EffectiveMessage.Message)
				if fileURL != "" {
					mimeType := utils.DetectMimeTypeFromURL(fileURL)
					file = &types.DocumentFile{FileName: "external_media", MimeType: mimeType, FileSize: 0}
					return b.sendMediaToUser(ctx, u, fileURL, file, isForwarded)
				}
			}
		}
		return b.sendReply(ctx, u, "Unsupported media type.")
	}

	fileURL := b.generateFileURL(u.EffectiveMessage.Message.ID, file)
	b.logger.Printf("Generated media URL: %s", fileURL)

	return b.sendMediaToUser(ctx, u, fileURL, file, isForwarded)
}

func (b *TelegramBot) isUserChat(ctx *ext.Context, chatID int64) bool {
	peerChatID := ctx.PeerStorage.GetPeerById(chatID)
	if peerChatID.Type != int(storage.TypeUser) {
		b.logger.Printf("Chat ID %d is not a user type.", chatID)
		return false
	}
	return true
}

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	if err != nil {
		b.logger.Printf("Failed to send reply: %v", err)
	}
	return err
}

// Mensaje de bienvenida sin botones
func (b *TelegramBot) sendMediaURLReply(ctx *ext.Context, u *ext.Update, msg, webURL string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	if err != nil {
		b.logger.Printf("Failed to send start message: %v", err)
	}
	return err
}

// Respuesta a medios: solo botón STREAMING
func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile, isForwarded bool) error {
	messageText := fileURL

	var keyboardRows []tg.KeyboardButtonRow
	if !strings.Contains(strings.ToLower(fileURL), "localhost") && !strings.Contains(strings.ToLower(fileURL), "127.0.0.1") {
		keyboardRows = append(keyboardRows, tg.KeyboardButtonRow{
			Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButtonURL{Text: "STREAMING", URL: fileURL},
			},
		})
	}

	_, err := ctx.Reply(u, ext.ReplyTextString(messageText), &ext.ReplyOpts{
		Markup: &tg.ReplyInlineMarkup{Rows: keyboardRows},
	})
	if err != nil {
		b.logger.Printf("Error sending media reply: %v", err)
		return err
	}

	wsMsg := b.constructWebSocketMessage(fileURL, file)
	b.webServer.GetWSManager().PublishMessage(u.EffectiveChat().GetID(), wsMsg)

	return nil
}

func (b *TelegramBot) constructWebSocketMessage(fileURL string, file *types.DocumentFile) map[string]string {
	proxiedURL := b.wrapWithProxyIfNeeded(fileURL)
	return map[string]string{
		"url":         proxiedURL,
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

func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	adminID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(adminID)
	if err != nil || !userInfo.IsAdmin {
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

	totalUsers, _ := b.userRepository.GetUserCount()
	offset := (page - 1) * pageSize
	users, _ := b.userRepository.GetAllUsers(offset, pageSize)

	if len(users) == 0 {
		return b.sendReply(ctx, u, "No users found.")
	}

	var msg strings.Builder
	msg.WriteString("User List\n\n")
	for i, user := range users {
		status := "Not Authorized"
		if user.IsAuthorized {
			status = "Authorized"
		}
		adminStatus := ""
		if user.IsAdmin {
			adminStatus = "Admin"
		}
		username := "N/A"
		if user.Username != "" {
			username = "@" + user.Username
		}
		msg.WriteString(fmt.Sprintf("%d. ID:%d %s %s (%s) - %s %s\n",
			offset+i+1, user.UserID, user.FirstName, user.LastName, username, status, adminStatus))
	}
	totalPages := (totalUsers + pageSize - 1) / pageSize
	msg.WriteString(fmt.Sprintf("\nPage %d of %d", page, totalPages))

	_, err = ctx.Reply(u, ext.ReplyTextString(msg.String()), &ext.ReplyOpts{})
	return err
}

func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	adminID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(adminID)
	if err != nil || !userInfo.IsAdmin {
		return b.sendReply(ctx, u, "You are not authorized to perform this action.")
	}

	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /userinfo <user_id>")
	}
	targetUserID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return b.sendReply(ctx, u, "Invalid user ID.")
	}

	targetUserInfo, err := b.userRepository.GetUserInfo(targetUserID)
	if err != nil {
		return b.sendReply(ctx, u, "User not found.")
	}

	status := "Not Authorized"
	if targetUserInfo.IsAuthorized {
		status = "Authorized"
	}
	adminStatus := "No"
	if targetUserInfo.IsAdmin {
		adminStatus = "Yes"
	}
	username := "N/A"
	if targetUserInfo.Username != "" {
		username = "@" + targetUserInfo.Username
	}

	msg := fmt.Sprintf(
		"User Details:\nID: %d\nName: %s %s\nUsername: %s\nStatus: %s\nAdmin: %s",
		targetUserInfo.UserID, targetUserInfo.FirstName, targetUserInfo.LastName, username, status, adminStatus,
	)

	_, err = ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	return err
}

func escapeMarkdownV2(text string) string {
	replacer := strings.NewReplacer(
		"_", "\\_", "*", "\\*", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)",
		"~", "\\~", "`", "\\`", ">", "\\>", "#", "\\#", "+", "\\+", "-", "\\-",
		"=", "\\=", "|", "\\|", "{", "\\{", "}", "\\}", ".", "\\.", "!", "\\!",
	)
	return replacer.Replace(text)
}
