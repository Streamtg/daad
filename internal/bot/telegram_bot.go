package bot

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"os"
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

// NewTelegramBot creates a new instance of TelegramBot.
func NewTelegramBot(config *config.Configuration, log *logger.Logger) (*TelegramBot, error) {
	dsn := fmt.Sprintf("file:%s?mode=rwc", config.DatabasePath)

	// restaurar DB del canal si no existe localmente
	if _, err := os.Stat(config.DatabasePath); os.IsNotExist(err) {
		log.Printf("Local DB not found, trying to download last backup…")
		if err := downloadDBFromLogChannel(config, log); err != nil {
			log.Printf("Could not download backup (%v), starting with empty DB", err)
		}
	}

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
	clientDispatcher.AddHandler(handlers.NewCommand("sms", b.handleSMS)) // <-- NUEVO
	clientDispatcher.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
	clientDispatcher.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
}

/* ----------  NUEVOS MÉTODOS  ---------- */

// handleSMS envía un mensaje de broadcast a todos los usuarios autorizados
func (b *TelegramBot) handleSMS(ctx *ext.Context, u *ext.Update) error {
	adminID := u.EffectiveUser().ID
	adminInfo, err := b.userRepository.GetUserInfo(adminID)
	if err != nil || !adminInfo.IsAdmin {
		return b.sendReply(ctx, u, "Only administrators can use this command.")
	}
	text := strings.TrimSpace(u.EffectiveMessage.Text)
	text = strings.TrimPrefix(text, "/sms")
	text = strings.TrimSpace(text)
	if text == "" {
		return b.sendReply(ctx, u, "Usage: /sms <message to broadcast>")
	}
	users, _ := b.userRepository.GetAllUsers(0, 0) // 0,0 → todos
	if len(users) == 0 {
		return b.sendReply(ctx, u, "No users to notify.")
	}
	sent := 0
	for _, usr := range users {
		if !usr.IsAuthorized {
			continue
		}
		peer := b.tgCtx.PeerStorage.GetInputPeerById(usr.ChatID)
		_, err := b.tgCtx.SendMessage(usr.ChatID, &tg.MessagesSendMessageRequest{
			Peer:    peer,
			Message: fmt.Sprintf("📢 *Broadcast*\n\n%s", text),
		})
		if err == nil {
			sent++
		}
	}
	return b.sendReply(ctx, u, fmt.Sprintf("Message sent to %d users.", sent))
}

// uploadDBToLogChannel sube la base de datos al canal de logs
func (b *TelegramBot) uploadDBToLogChannel(comment string) {
	if b.config.LogChannelID == "" || b.config.LogChannelID == "0" {
		return
	}
	f, err := os.Open(b.config.DatabasePath)
	if err != nil {
		b.logger.Printf("backup: cannot open DB file: %v", err)
		return
	}
	defer f.Close()

	up, err := b.tgClient.Client().UploadFile(b.tgCtx, f, b.config.DatabasePath, 512*1024)
	if err != nil {
		b.logger.Printf("backup: upload error: %v", err)
		return
	}

	media := &tg.InputMediaUploadedDocument{
		File:       up,
		MimeType:   "application/x-sqlite3",
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: fmt.Sprintf("base_%d.db", time.Now().Unix())}},
	}

	logChannelID, _ := strconv.ParseInt(b.config.LogChannelID, 10, 64)
	peer := b.tgCtx.PeerStorage.GetInputPeerById(logChannelID)
	_, err = b.tgClient.Client().MessagesSendMedia(b.tgCtx, &tg.MessagesSendMediaRequest{
		Peer:    peer,
		Media:   media,
		Message: comment,
	})
	if err != nil {
		b.logger.Printf("backup: sendMedia error: %v", err)
	}
}

// downloadDBFromLogChannel descarga la última copia del canal
func downloadDBFromLogChannel(cfg *config.Configuration, log *logger.Logger) error {
	tmpClient, err := gotgproto.NewClient(
		cfg.ApiID,
		cfg.ApiHash,
		gotgproto.ClientTypeBot(cfg.BotToken),
		&gotgproto.ClientOpts{InMemory: true},
	)
	if err != nil {
		return err
	}
	defer tmpClient.Stop()

	logChannelID, _ := strconv.ParseInt(cfg.LogChannelID, 10, 64)
	if logChannelID == 0 {
		return fmt.Errorf("LOG_CHANNEL_ID not configured")
	}

	ctx := tmpClient.CreateContext()
	peer := ctx.PeerStorage.GetInputPeerById(logChannelID)

	resp, err := tmpClient.Client().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:  peer,
		Limit: 20,
	})
	if err != nil {
		return fmt.Errorf("cannot fetch history: %w", err)
	}
	msgs, ok := resp.(*tg.MessagesMessagesSlice)
	if !ok {
		return fmt.Errorf("unexpected history type")
	}

	var lastDoc *tg.Message
	for _, m := range msgs.Messages {
		msg, ok := m.(*tg.Message)
		if !ok || msg.Media == nil {
			continue
		}
		if _, ok := msg.Media.(*tg.MessageMediaDocument); ok {
			lastDoc = msg
			break
		}
	}
	if lastDoc == nil {
		return fmt.Errorf("no DB document found in channel")
	}

	media := lastDoc.Media.(*tg.MessageMediaDocument)
	buf := &bytes.Buffer{}
	if _, err := tmpClient.Client().DownloadFile(ctx, media.Document, buf); err != nil {
		return fmt.Errorf("download error: %w", err)
	}

	out, err := os.Create(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("create file error: %w", err)
	}
	defer out.Close()
	if _, err := io.Copy(out, buf); err != nil {
		return fmt.Errorf("save file error: %w", err)
	}
	log.Printf("Database restored from channel backup")
	return nil
}

/* ----------  MÉTODOS EXISTENTES (SIN CAMBIOS)  ---------- */

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

	startMsg := fmt.Sprintf(
		"Hello Streammgram, I am @Mediaprocesor_bot, your bridge between Telegram and the Web!\n\n" +
			"You can **forward** or **directly upload** media files (audio, video, photos, or documents) to this bot.\n" +
			"I will instantly generate a streaming link and play it on your web player.\n\n" +
			"**Features:**\n" +
			"• Forward media from any chat\n" +
			"• Upload media directly (including video files as documents)\n" +
			"• Instant web streaming",
	)

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

	go b.uploadDBToLogChannel("DB backup – deauthorize user") // <-- BACKUP

	return b.sendReply(ctx, u, fmt.Sprintf("User %d has been deauthorized.", targetUserID))
}

func (b *TelegramBot) handleAnyUpdate(ctx *ext.Context, u *ext.Update) error {
	// (tu código existente sin cambios)
	return nil
}

func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	// (tu código existente sin cambios)
	return nil
}

func (b *TelegramBot) isUserChat(ctx *ext.Context, chatID int64) bool {
	// (tu código existente sin cambios)
	return true
}

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	// (tu código existente sin cambios)
	return nil
}

func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile, isForwarded bool) error {
	// (tu código existente sin cambios)
	return nil
}

func (b *TelegramBot) constructWebSocketMessage(fileURL string, file *types.DocumentFile) map[string]string {
	// (tu código existente sin cambios)
	return nil
}

func (b *TelegramBot) generateFileURL(messageID int, file *types.DocumentFile) string {
	// (tu código existente sin cambios)
	return ""
}

func (b *TelegramBot) wrapWithProxyIfNeeded(fileURL string) string {
	// (tu código existente sin cambios)
	return ""
}

func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	// (tu código existente sin cambios)
	return nil
}

func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	// (tu código existente sin cambios)
	return nil
}

func escapeMarkdownV2(text string) string {
	// (tu código existente sin cambios)
	return ""
}
