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

	// Initialize the database connection
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open SQLite database: %w", err)
	}

	// Create a new UserRepository
	userRepository := data.NewUserRepository(db)

	// Initialize the database schema
	if err := userRepository.InitDB(); err != nil {
		return nil, err
	}

	tgCtx := tgClient.CreateContext()

	// Create web server
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
	clientDispatcher.AddHandler(handlers.NewCommand("deauthorize", b.handleDeauthorizeUser))
	clientDispatcher.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
	clientDispatcher.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))
	clientDispatcher.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
	clientDispatcher.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
}

// handleStartCommand envía el mensaje de bienvenida **sin botones** y **sin enlace**
func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()
	user := u.EffectiveUser()

	if user.ID == ctx.Self.ID {
		b.logger.Printf("Ignoring /start command from bot's own ID (%d).", user.ID)
		return nil
	}

	b.logger.Printf("Received /start command from user: %s (ID: %d) in chat: %d", user.FirstName, user.ID, chatID)

	if b.config.DebugMode {
		b.logger.Debugf("/start command - User: %s %s, Username: @%s, ChatID: %d",
			user.FirstName, user.LastName, user.Username, chatID)
	}

	existingUser, err := b.userRepository.GetUserInfo(user.ID)
	if err != nil {
		if err == sql.ErrNoRows {
			b.logger.Printf("User %d not found in DB, attempting to register.", user.ID)
			existingUser = nil
		} else {
			b.logger.Printf("Failed to retrieve user info from DB for /start: %v", err)
			return fmt.Errorf("failed to retrieve user info for start command: %w", err)
		}
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
		b.logger.Printf("User %d already exists in DB with isAuthorized=%t, isAdmin=%t", user.ID, isAuthorized, isAdmin)
	}

	// Mensaje de bienvenida **exacto** como lo pediste
	startMsg := fmt.Sprintf(
		"Hello Streammgram, I am @Mediaprocesor_bot, your bridge between Telegram and the Web!\n\n"+
			"You can **forward** or **directly upload** media files (audio, video, photos, or documents) to this bot.\n"+
			"I will instantly generate a streaming link and play it on your web player.\n\n"+
			"**Features:**\n"+
			"• Forward media from any chat\n"+
			"• Upload media directly (including video files as documents)\n"+
			"• Instant web streaming",
	)

	// Enviamos **sin botones** y **sin enlace**
	err = b.sendReply(ctx, u, startMsg)
	if err != nil {
		b.logger.Printf("Failed to send start message to user %d: %v", user.ID, err)
		return fmt.Errorf("failed to send start message: %w", err)
	}

	if !isAuthorized {
		b.logger.Printf("DEBUG: User %d is NOT authorized (isAuthorized=%t). Sending unauthorized message.", user.ID, isAuthorized)
		authorizationMsg := "You are not authorized to use this bot yet. Please ask one of the administrators to authorize you and wait until you receive a confirmation."
		return b.sendReply(ctx, u, authorizationMsg)
	}

	b.logger.Printf("DEBUG: User %d is authorized. /start command completed successfully.", user.ID)
	return nil
}

// notifyAdminsAboutNewUser sends a notification to all admins about the new user.
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
					&tg.KeyboardButtonCallback{Text: "Authorize", Data: []byte(fmt.Sprintf("auth,%d,authorize", newUser.ID))},
					&tg.KeyboardButtonCallback{Text: "Decline", Data: []byte(fmt.Sprintf("auth,%d,decline", newUser.ID))},
				},
			},
		},
	}

	for _, admin := range admins {
		if admin.UserID == newUser.ID && admin.UserID == newUsersChatID {
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
	if err != nil {
		b.logger.Printf("Failed to retrieve user info for admin check: %v", err)
		return b.sendReply(ctx, u, "Failed to authorize the user.")
	}

	if !userInfo.IsAdmin {
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
	} else {
		b.logger.Printf("Could not get user info for user %d: %v", targetUserID, err)
	}

	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been authorized%s.", targetUserID, adminMsgSuffix))
}

func (b *TelegramBot) handleDeauthorizeUser(ctx *ext.Context, u *ext.Update) error {
	b.logger.Printf("Received /deauthorize command from user ID: %d", u.EffectiveUser().ID)

	adminID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(adminID)
	if err != nil {
		b.logger.Printf("Failed to retrieve user info for admin check: %v", err)
		return b.sendReply(ctx, u, "Failed to deauthorize the user.")
	}

	if !userInfo.IsAdmin {
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
	} else {
		b.logger.Printf("Could not get user info for user %d: %v", targetUserID, err)
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

			b.logger.Debugf("Message ID: %d, Date: %d", message.Message.ID, message.Message.Date)

			if fwdFrom, isForwarded := message.Message.GetFwdFrom(); isForwarded {
				b.logger.Debugf("FORWARDED message - Original date: %d, FromID: %v, FromName: %s",
					fwdFrom.Date, fwdFrom.FromID, fwdFrom.FromName)
			}

			if message.Text != "" {
				textPreview := message.Text
				if len(textPreview) > 100 {
					textPreview = textPreview[:100] + "..."
				}
				b.logger.Debugf("Text message: \"%s\"", textPreview)
			}

			if message.Message.Media != nil {
				mediaType := fmt.Sprintf("%T", message.Message.Media)
				b.logger.Debugf("Media attached - Type: %s", mediaType)

				switch media := message.Message.Media.(type) {
				case *tg.MessageMediaDocument:
					if doc, ok := media.Document.AsNotEmpty(); ok {
						b.logger.Debugf("   Document ID: %d, Size: %d bytes, MimeType: %s",
							doc.ID, doc.Size, doc.MimeType)
					}
				case *tg.MessageMediaPhoto:
					if photo, ok := media.Photo.AsNotEmpty(); ok {
						b.logger.Debugf("   Photo ID: %d, HasStickers: %t",
							photo.ID, photo.HasStickers)
					}
				}
			}

			if replyTo, ok := message.Message.GetReplyTo(); ok {
				if replyMsg, ok := replyTo.(*tg.MessageReplyHeader); ok {
					b.logger.Debugf("Reply to message ID: %d", replyMsg.ReplyToMsgID)
				}
			}

			if markup, ok := message.Message.GetReplyMarkup(); ok {
				b.logger.Debugf("Message has reply markup: %T", markup)
			}
		}

		if u.CallbackQuery != nil {
			b.logger.Debugf("Callback query from user %d: %s",
				u.CallbackQuery.UserID, string(u.CallbackQuery.Data))
		}
	}

	return nil
}

func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()
	user := u.EffectiveUser()

	fwdHeader, isForwarded := u.EffectiveMessage.Message.GetFwdFrom()
	messageType := "direct upload"
	if isForwarded {
		messageType = "forwarded message"
		if b.config.DebugMode {
			b.logger.Debugf("Forwarded message detected - Date: %d, FromID: %v, FromName: %s",
				fwdHeader.Date,
				fwdHeader.FromID,
				fwdHeader.FromName)
		}
	}

	b.logger.Printf("Received media %s from user: %s (ID: %d) in chat: %d", messageType, user.FirstName, user.ID, chatID)

	if b.config.DebugMode {
		b.logger.Debugf("Message ID: %d, Media Type: %T", u.EffectiveMessage.Message.ID, u.EffectiveMessage.Message.Media)
	}

	if !b.isUserChat(ctx, chatID) {
		return dispatcher.EndGroups
	}

	existingUser, err := b.userRepository.GetUserInfo(chatID)
	if err != nil {
		if err == sql.ErrNoRows {
			b.logger.Printf("User %d not in DB for media message, sending unauthorized message.", chatID)
			authorizationMsg := "You are not authorized to use this bot yet. Please ask one of the administrators to authorize you and wait until you receive a confirmation."
			return b.sendReply(ctx, u, authorizationMsg)
		}
		b.logger.Printf("Failed to retrieve user info from DB for media message for user %d: %v", chatID, err)
		return fmt.Errorf("failed to retrieve user info for media handling: %w", err)
	}

	b.logger.Printf("User %d retrieved for media message. isAuthorized=%t, isAdmin=%t", chatID, existingUser.IsAuthorized, existingUser.IsAdmin)

	if !existingUser.IsAuthorized {
		b.logger.Printf("DEBUG: User %d is NOT authorized (isAuthorized=%t). Sending unauthorized message for media.", chatID, existingUser.IsAuthorized)
		authorizationMsg := "You are not authorized to use this bot yet. Please ask one of the administrators to authorize you and wait until you receive a confirmation."
		return b.sendReply(ctx, u, authorizationMsg)
	}

	if b.config.LogChannelID != "" && b.config.LogChannelID != "0" {
		if b.config.DebugMode {
			b.logger.Debugf("Log channel configured: %s. Starting message forwarding in background.", b.config.LogChannelID)
		}
		go func() {
			fromChatID := u.EffectiveChat().GetID()
			messageID := u.EffectiveMessage.Message.ID

			updates, err := utils.ForwardMessages(ctx, fromChatID, b.config.LogChannelID, messageID)
			if err != nil {
				b.logger.Printf("Failed to forward message %d from chat %d to log channel %s: %v", messageID, fromChatID, b.config.LogChannelID, err)
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
				b.logger.Printf("Could not find new message ID in forward-updates for original msg %d", messageID)
				return
			}

			userInfo, err := b.userRepository.GetUserInfo(fromChatID)
			if err != nil {
				b.logger.Printf("Could not get user info for user %d to send to log channel", fromChatID)
				return
			}

			var usernameDisplay string
			if userInfo.Username != "" {
				usernameDisplay = "@" + userInfo.Username
			} else {
				usernameDisplay = "N/A"
			}

			infoMsg := fmt.Sprintf("Media from user:\nID: %d\nName: %s %s\nUsername: %s",
				userInfo.UserID,
				userInfo.FirstName,
				userInfo.LastName,
				usernameDisplay,
			)

			logChannelPeer, err := utils.GetLogChannelPeer(ctx, b.config.LogChannelID)
			if err != nil {
				b.logger.Printf("Failed to get log channel peer %s to send reply: %v", b.config.LogChannelID, err)
				return
			}

			_, err = ctx.Raw.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer:     logChannelPeer,
				Message:  infoMsg,
				ReplyTo:  &tg.InputReplyToMessage{ReplyToMsgID: newMsgID},
				RandomID: rand.Int63(),
			})
			if err != nil {
				b.logger.Printf("Failed to send user info to log channel %s as reply: %v", b.config.LogChannelID, err)
			}
		}()
	}

	if b.config.DebugMode {
		b.logger.Debugf("Attempting to extract file information from media for message ID %d", u.EffectiveMessage.Message.ID)
	}

	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		if webPageMedia, ok := u.EffectiveMessage.Message.Media.(*tg.MessageMediaWebPage); ok {
			if _, isEmpty := webPageMedia.Webpage.(*tg.WebPageEmpty); isEmpty {
				fileURL := utils.ExtractURLFromEntities(u.EffectiveMessage.Message)
				if fileURL != "" {
					isFileHosting := strings.Contains(strings.ToLower(fileURL), "attach.fahares.com") ||
						strings.Contains(strings.ToLower(fileURL), "filehosting") ||
						strings.Contains(strings.ToLower(fileURL), "upload")

					mimeType := utils.DetectMimeTypeFromURL(fileURL)
					file = &types.DocumentFile{
						FileName: "external_media",
						MimeType: mimeType,
						FileSize: 0,
					}

					err := b.sendMediaToUser(ctx, u, fileURL, file, isForwarded)

					if err == nil && isFileHosting {
						warningMsg := "Note: This appears to be a file hosting page. If the media doesn't play, please:\n" +
							"• Send the file directly (not forwarded)\n" +
							"• Or provide a direct download link"
						time.Sleep(500 * time.Millisecond)
						_ = b.sendReply(ctx, u, warningMsg)
					}

					return err
				}
			}
		}

		b.logger.Printf("Error processing media message from chat ID %d, message ID %d: %v", chatID, u.EffectiveMessage.Message.ID, err)
		return b.sendReply(ctx, u, fmt.Sprintf("Unsupported media type or error processing file: %v", err))
	}

	fileURL := b.generateFileURL(u.EffectiveMessage.Message.ID, file)
	b.logger.Printf("Generated media file URL for message ID %d in chat ID %d: %s (forwarded: %t)", u.EffectiveMessage.Message.ID, chatID, fileURL, isForwarded)

	return b.sendMediaToUser(ctx, u, fileURL, file, isForwarded)
}

func (b *TelegramBot) isUserChat(ctx *ext.Context, chatID int64) bool {
	peerChatID := ctx.PeerStorage.GetPeerById(chatID)
	if peerChatID.Type != int(storage.TypeUser) {
		b.logger.Printf("Chat ID %d is not a user type. Terminating processing.", chatID)
		return false
	}
	return true
}

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	if err != nil {
		b.logger.Printf("Failed to send reply to user: %s (ID: %d) - Error: %v", u.EffectiveUser().FirstName, u.EffectiveUser().ID, err)
	}
	return err
}

// sendMediaURLReply ya no se usa (se reemplazó por sendReply en /start)
func (b *TelegramBot) sendMediaURLReply(ctx *ext.Context, u *ext.Update, msg, webURL string) error {
	return b.sendReply(ctx, u, msg)
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
		b.logger.Printf("Error sending reply for chat ID %d, message ID %d: %v", u.EffectiveChat().GetID(), u.EffectiveMessage.Message.ID, err)
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
	hash := utils.GetShortHash(utils.PackFile(
		file.FileName,
		file.FileSize,
		file.MimeType,
		file.ID,
	), b.config.HashLength)
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

// handleListUsers lists all users in a paginated format
func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	b.logger.Printf("Received /listusers command from user ID: %d", u.EffectiveUser().ID)

	adminID := u.EffectiveUser().ID
	userInfo, err := b.userRepository.GetUserInfo(adminID)
	if err != nil || !userInfo.IsAdmin {
		return b.sendReply(ctx, u, "You are not authorized to perform this action.")
	}

	const pageSize = 10
	page := 1
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) > 1 {
		parsedPage, err := strconv.Atoi(args[1])
		if err == nil && parsedPage > 0 {
			page = parsedPage
		}
	}

	totalUsers, err := b.userRepository.GetUserCount()
	if err != nil {
		b.logger.Printf("Failed to get user count: %v", err)
		return b.sendReply(ctx, u, "Error retrieving user count.")
	}

	offset := (page - 1) * pageSize
	users, err := b.userRepository.GetAllUsers(offset, pageSize)
	if err != nil {
		b.logger.Printf("Failed to get users for listing: %v", err)
		return b.sendReply(ctx, u, "Error retrieving user list.")
	}

	if len(users) == 0 {
		return b.sendReply(ctx, u, "No users found or page is empty.")
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
		username := user.Username
		if username == "" {
			username = "N/A"
		}
		msg.WriteString(fmt.Sprintf("%d. ID:%d %s %s (@%s) - Auth: %s Admin: %s\n",
			offset+i+1, user.UserID, user.FirstName, user.LastName, username, status, adminStatus))
	}

	totalPages := (totalUsers + pageSize - 1) / pageSize
	msg.WriteString(fmt.Sprintf("\nPage %d of %d (%d total users)", page, totalPages, totalUsers))

	_, err = ctx.Reply(u, ext.ReplyTextString(msg.String()), &ext.ReplyOpts{})
	return err
}

// handleUserInfo retrieves detailed information about a specific user.
func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	b.logger.Printf("Received /userinfo command from user ID: %d", u.EffectiveUser().ID)

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
		if err == sql.ErrNoRows {
			return b.sendReply(ctx, u, fmt.Sprintf("User with ID %d not found.", targetUserID))
		}
		b.logger.Printf("Failed to get user info for ID %d: %v", targetUserID, err)
		return b.sendReply(ctx, u, "Error retrieving user information.")
	}

	status := "Not Authorized"
	if targetUserInfo.IsAuthorized {
		status = "Authorized"
	}
	adminStatus := "No"
	if targetUserInfo.IsAdmin {
		adminStatus = "Yes"
	}

	username := targetUserInfo.Username
	if username == "" {
		username = "N/A"
	}

	msg := fmt.Sprintf(
		"User Details:\n"+
			"ID: %d\n"+
			"Chat ID: %d\n"+
			"First Name: %s last Name: %s\n"+
			"Username: @%s\n"+
			"Status: %s\n"+
			"Admin: %s\n"+
			"Joined: %s",
		targetUserInfo.UserID,
		targetUserInfo.ChatID,
		targetUserInfo.FirstName,
		targetUserInfo.LastName,
		username,
		status,
		adminStatus,
		targetUserInfo.CreatedAt,
	)

	_, err = ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	return err
}

// escapeMarkdownV2 escapes characters that have special meaning in Telegram MarkdownV2.
func escapeMarkdownV2(text string) string {
	replacer := strings.NewReplacer(
		"_", "\\_", "*", "\\*", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)",
		"~", "\\~", "`", "\\`", ">", "\\>", "#", "\\#", "+", "\\+", "-", "\\-",
		"=", "\\=", "|", "\\|", "{", "\\{", "}", "\\}", ".", "\\.", "!", "\\!",
	)
	return replacer.Replace(text)
}
