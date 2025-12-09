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

type TelegramBot struct {
	config         *config.Configuration
	tgClient       *gotgproto.Client
	tgCtx          *ext.Context
	logger         *logger.Logger
	userRepository *data.UserRepository
	db             *sql.DB
	webServer      *web.Server
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
	clientDispatcher := b.tgClient.Dispatcher
	clientDispatcher.AddHandler(handlers.NewCommand("start", b.handleStartCommand))
	clientDispatcher.AddHandler(handlers.NewCommand("authorize", b.handleAuthorizeUser))
	clientDispatcher.AddHandler(handlers.NewCommand("deauthorize", b.handleDeauthorizeUser))
	clientDispatcher.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
	clientDispatcher.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))
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

	existingUser, err := b.userRepository.GetUserInfo(user.ID)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("failed to retrieve user info: %w", err)
	}

	isFirstUser, _ := b.userRepository.IsFirstUser()
	isAdmin := false
	isAuthorized := false

	if existingUser == nil {
		if isFirstUser {
			isAuthorized = true
			isAdmin = true
		}
		err = b.userRepository.StoreUserInfo(user.ID, chatID, user.FirstName, user.LastName, user.Username, isAuthorized, isAdmin)
		if err != nil {
			return fmt.Errorf("failed to store user info: %w", err)
		}
		if !isAdmin {
			go b.notifyAdminsAboutNewUser(user, chatID)
		}
	} else {
		isAuthorized = existingUser.IsAuthorized
		isAdmin = existingUser.IsAdmin
	}

	startMsg := fmt.Sprintf(
		"Hello Streammgram, I am @Mediaprocesor_bot, your bridge between Telegram and the Web!\n\n"+
			"You can **forward** or **directly upload** media files (audio, video, photos, or documents) to this bot.\n"+
			"I will instantly generate a streaming link and play it on your web player.\n\n"+
			"**Features:**\n"+
			"• Forward media from any chat\n"+
			"• Upload media directly (including video files as documents)\n"+
			"• Instant web streaming",
	)

	if err := b.sendReply(ctx, u, startMsg); err != nil {
		return err
	}

	if !isAuthorized {
		return b.sendReply(ctx, u, "You are not authorized to use this bot yet. Please ask one of the administrators to authorize you and wait until you receive a confirmation.")
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
					&tg.KeyboardButtonCallback{Text: "Authorize", Data: []byte(fmt.Sprintf("auth,%d,authorize", newUser.ID))},
					&tg.KeyboardButtonCallback{Text: "Decline", Data: []byte(fmt.Sprintf("auth,%d,decline", newUser.ID))},
				},
			},
		},
	}

	for _, admin := range admins {
		if admin.UserID == newUser.ID {
			continue
		}
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
	if err := b.userRepository.AuthorizeUser(targetUserID, isAdmin); err != nil {
		return b.sendReply(ctx, u, "Failed to authorize the user.")
	}

	targetUserInfo, _ := b.userRepository.GetUserInfo(targetUserID)
	if targetUserInfo != nil && targetUserInfo.ChatID != 0 {
		peer := b.tgCtx.PeerStorage.GetInputPeerById(targetUserInfo.ChatID)
		msg := "You have been authorized to use WebBridgeBot!"
		if isAdmin {
			msg += " (as admin)"
		}
		b.tgCtx.SendMessage(targetUserInfo.ChatID, &tg.MessagesSendMessageRequest{Peer: peer, Message: msg})
	}

	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been authorized.", targetUserID))
}

func (b *TelegramBot) handleDeauthorizeUser(ctx *ext.Context, u *ext.Update) error {
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

	if err := b.userRepository.DeauthorizeUser(targetUserID); err != nil {
		return b.sendReply(ctx, u, "Failed to deauthorize the user.")
	}

	targetUserInfo, _ := b.userRepository.GetUserInfo(targetUserID)
	if targetUserInfo != nil && targetUserInfo.ChatID != 0 {
		peer := b.tgCtx.PeerStorage.GetInputPeerById(targetUserInfo.ChatID)
		b.tgCtx.SendMessage(targetUserInfo.ChatID, &tg.MessagesSendMessageRequest{
			Peer:    peer,
			Message: "You have been deauthorized from using WebBridgeBot.",
		})
	}

	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been deauthorized.", targetUserID))
}

func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error {
	return nil
}

// REENVÍO AL CANAL DE LOGS (TU LÓGICA ORIGINAL QUE SÍ FUNCIONA)
func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()
	user := u.EffectiveUser()

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

	// TU LÓGICA DE REENVÍO AL CANAL DE LOGS (100% funcional)
	if b.config.LogChannelID != "" && b.config.LogChannelID != "0" {
		go func() {
			fromChatID := u.EffectiveChat().GetID()
			messageID := u.EffectiveMessage.Message.ID

			updates, err := utils.ForwardMessages(ctx, fromChatID, b.config.LogChannelID, messageID)
			if err != nil {
				b.logger.Printf("Failed to forward message %d to log channel %s: %v", messageID, b.config.LogChannelID, err)
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
				b.logger.Printf("Could not find forwarded message ID")
				return
			}

			userInfo, err := b.userRepository.GetUserInfo(fromChatID)
			if err != nil {
				b.logger.Printf("Could not get user info for log: %v", err)
				return
			}

			usernameDisplay := "N/A"
			if userInfo.Username != "" {
				usernameDisplay = "@" + userInfo.Username
			}

			infoMsg := fmt.Sprintf("Media from user:\nID: %d\nName: %s %s\nUsername: %s",
				userInfo.UserID,
				userInfo.FirstName,
				userInfo.LastName,
				usernameDisplay,
			)

			logChannelPeer, err := utils.GetLogChannelPeer(ctx, b.config.LogChannelID)
			if err != nil {
				b.logger.Printf("Failed to get log channel peer: %v", err)
				return
			}

			_, err = ctx.Raw.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer:     logChannelPeer,
				Message:  infoMsg,
				ReplyTo:  &tg.InputReplyToMessage{ReplyToMsgID: newMsgID},
				RandomID: rand.Int63(),
			})
			if err != nil {
				b.logger.Printf("Failed to send user info to log channel: %v", err)
			}
		}()
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

					err := b.sendMediaToUser(ctx, u, fileURL, file, false)
					if err == nil && isFileHosting {
						time.Sleep(500 * time.Millisecond)
						b.sendReply(ctx, u, "Note: This appears to be a file hosting page. If the media doesn't play, please:\n• Send the file directly (not forwarded)\n• Or provide a direct download link")
					}
					return err
				}
			}
		}
		return b.sendReply(ctx, u, fmt.Sprintf("Unsupported media type: %v", err))
	}

	fileURL := b.generateFileURL(u.EffectiveMessage.Message.ID, file)
	return b.sendMediaToUser(ctx, u, fileURL, file, false)
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
		b.logger.Printf("Error sending reply: %v", err)
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
		parsedPage, err := strconv.Atoi(args[1])
		if err == nil && parsedPage > 0 {
			page = parsedPage
		}
	}

	totalUsers, err := b.userRepository.GetUserCount()
	if err != nil {
		return b.sendReply(ctx, u, "Error retrieving user count.")
	}

	offset := (page - 1) * pageSize
	users, err := b.userRepository.GetAllUsers(offset, pageSize)
	if err != nil {
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
		if err == sql.ErrNoRows {
			return b.sendReply(ctx, u, fmt.Sprintf("User with ID %d not found.", targetUserID))
		}
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

func escapeMarkdownV2(text string) string {
	replacer := strings.NewReplacer(
		"_", "\\_", "*", "\\*", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)",
		"~", "\\~", "`", "\\`", ">", "\\>", "#", "\\#", "+", "\\+", "-", "\\-",
		"=", "\\=", "|", "\\|", "{", "\\{", "}", "\\}", ".", "\\.", "!", "\\!",
	)
	return replacer.Replace(text)
}
