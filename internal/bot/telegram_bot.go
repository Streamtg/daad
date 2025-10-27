package bot

import (
	"database/sql"
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"strings"

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
	d := b.tgClient.Dispatcher
	d.AddHandler(handlers.NewCommand("start", b.handleStartCommand))
	d.AddHandler(handlers.NewCommand("authorize", b.handleAuthorizeUser))
	d.AddHandler(handlers.NewCommand("deauthorize", b.handleDeauthorizeUser))
	d.AddHandler(handlers.NewCommand("listusers", b.handleListUsers))
	d.AddHandler(handlers.NewCommand("userinfo", b.handleUserInfo))
	d.AddHandler(handlers.NewAnyUpdate(b.handleAnyUpdate))
	d.AddHandler(handlers.NewMessage(filters.Message.Media, b.handleMediaMessages))
}

func (b *TelegramBot) handleStartCommand(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()
	user := u.EffectiveUser()

	if user.ID == ctx.Self.ID {
		return nil
	}

	existingUser, _ := b.userRepository.GetUserInfo(user.ID)
	isFirstUser, _ := b.userRepository.IsFirstUser()

	isAdmin := false
	isAuthorized := false

	if existingUser == nil {
		if isFirstUser {
			isAuthorized = true
			isAdmin = true
		}
		_ = b.userRepository.StoreUserInfo(user.ID, chatID, user.FirstName, user.LastName, user.Username, isAuthorized, isAdmin)
		if !isAdmin {
			go b.notifyAdminsAboutNewUser(user, chatID)
		}
	} else {
		isAuthorized = existingUser.IsAuthorized
		isAdmin = existingUser.IsAdmin
	}

	webURL := fmt.Sprintf("%s/%d", b.config.BaseURL, chatID)
	msg := fmt.Sprintf(
		"Hello %s, I am @%s!\n\n"+
			"Send or forward any media (audio, video, photo, document).\n"+
			"It will instantly appear on your web player.\n\n"+
			"Your player: %s",
		user.FirstName, ctx.Self.Username, webURL,
	)

	if err := b.sendMediaURLReply(ctx, u, msg, webURL); err != nil {
		return err
	}

	if !isAuthorized {
		return b.sendReply(ctx, u, "You are not authorized yet. Ask an admin to /authorize you.")
	}
	return nil
}

func (b *TelegramBot) notifyAdminsAboutNewUser(newUser *tg.User, chatID int64) {
	admins, _ := b.userRepository.GetAllAdmins()
	username := "@" + newUser.Username
	if newUser.Username == "" {
		username = "N/A"
	}
	msg := fmt.Sprintf("New user:\n*%s %s* (%s)\nID: `%d`", newUser.FirstName, newUser.LastName, username, newUser.ID)

	markup := &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{Text: "Authorize", Data: []byte(fmt.Sprintf("auth,%d,1", newUser.ID))},
					&tg.KeyboardButtonCallback{Text: "Decline", Data: []byte(fmt.Sprintf("auth,%d,0", newUser.ID))},
				},
			},
		},
	}

	for _, admin := range admins {
		if admin.UserID == newUser.ID {
			continue
		}
		peer := b.tgCtx.PeerStorage.GetInputPeerById(admin.ChatID)
		b.tgCtx.SendMessage(admin.ChatID, &tg.MessagesSendMessageRequest{Peer: peer, Message: msg, ReplyMarkup: markup})
	}
}

func (b *TelegramBot) handleAuthorizeUser(ctx *ext.Context, u *ext.Update) error {
	if admin, _ := b.userRepository.GetUserInfo(u.EffectiveUser().ID); admin == nil || !admin.IsAdmin {
		return b.sendReply(ctx, u, "Admins only.")
	}
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /authorize <user_id> [admin]")
	}
	id, _ := strconv.ParseInt(args[1], 10, 64)
	isAdmin := len(args) > 2 && args[2] == "admin"
	b.userRepository.AuthorizeUser(id, isAdmin)
	return b.sendReply(ctx, u, fmt.Sprintf("User %d authorized.", id))
}

func (b *TelegramBot) handleDeauthorizeUser(ctx *ext.Context, u *ext.Update) error {
	if admin, _ := b.userRepository.GetUserInfo(u.EffectiveUser().ID); admin == nil || !admin.IsAdmin {
		return b.sendReply(ctx, u, "Admins only.")
	}
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /deauthorize <user_id>")
	}
	id, _ := strconv.ParseInt(args[1], 10, 64)
	b.userRepository.DeauthorizeUser(id)
	return b.sendReply(ctx, u, fmt.Sprintf("User %d deauthorized.", id))
}

func (b *TelegramBot) handleAnyUpdate(*ext.Context, *ext.Update) error { return nil }

func (b *TelegramBot) handleMediaMessages(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()

	if !b.isUserChat(ctx, chatID) {
		return dispatcher.EndGroups
	}

	userInfo, _ := b.userRepository.GetUserInfo(chatID)
	if userInfo == nil || !userInfo.IsAuthorized {
		return b.sendReply(ctx, u, "Not authorized. Ask an admin.")
	}

	// Log channel (optional)
	if b.config.LogChannelID != "" && b.config.LogChannelID != "0" {
		go b.forwardToLogChannel(ctx, u)
	}

	file, err := utils.FileFromMedia(u.EffectiveMessage.Message.Media)
	if err != nil {
		if url := utils.ExtractURLFromEntities(u.EffectiveMessage.Message); url != "" {
			mime := utils.DetectMimeTypeFromURL(url)
			file = &types.DocumentFile{FileName: "external", MimeType: mime}
			return b.sendMediaToUser(ctx, u, url, file, false)
		}
		return b.sendReply(ctx, u, "Unsupported media.")
	}

	fileURL := b.generateFileURL(u.EffectiveMessage.Message.ID, file)
	return b.sendMediaToUser(ctx, u, fileURL, file, false)
}

func (b *TelegramBot) forwardToLogChannel(ctx *ext.Context, u *ext.Update) {
	fromID := u.EffectiveChat().GetID()
	msgID := u.EffectiveMessage.Message.ID
	updates, _ := utils.ForwardMessages(ctx, fromID, b.config.LogChannelID, msgID)

	var newMsgID int
	for _, upd := range updates.GetUpdates() {
		if m, ok := upd.(*tg.UpdateNewChannelMessage); ok {
			if msg, ok := m.Message.(*tg.Message); ok {
				newMsgID = msg.ID
				break
			}
		}
	}
	if newMsgID == 0 {
		return
	}

	user, _ := b.userRepository.GetUserInfo(fromID)
	info := fmt.Sprintf("From: %s %s (@%s)", user.FirstName, user.LastName, user.Username)
	peer, _ := utils.GetLogChannelPeer(ctx, b.config.LogChannelID)
	ctx.Raw.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     peer,
		Message:  info,
		ReplyTo:  &tg.InputReplyToMessage{ReplyToMsgID: newMsgID},
		RandomID: rand.Int63(),
	})
}

func (b *TelegramBot) isUserChat(ctx *ext.Context, chatID int64) bool {
	return ctx.PeerStorage.GetPeerById(chatID).Type == int(storage.TypeUser)
}

func (b *TelegramBot) sendReply(ctx *ext.Context, u *ext.Update, msg string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	return err
}

// /start: sin botones
func (b *TelegramBot) sendMediaURLReply(ctx *ext.Context, u *ext.Update, msg, _ string) error {
	_, err := ctx.Reply(u, ext.ReplyTextString(msg), &ext.ReplyOpts{})
	return err
}

// Respuesta a medios: solo botón STREAMING
func (b *TelegramBot) sendMediaToUser(ctx *ext.Context, u *ext.Update, fileURL string, file *types.DocumentFile, _ bool) error {
	var rows []tg.KeyboardButtonRow
	if !strings.Contains(strings.ToLower(fileURL), "localhost") && !strings.Contains(strings.ToLower(fileURL), "127.0.0.1") {
		rows = append(rows, tg.KeyboardButtonRow{
			Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButtonURL{Text: "STREAMING", URL: fileURL},
			},
		})
	}

	_, err := ctx.Reply(u, ext.ReplyTextString(fileURL), &ext.ReplyOpts{
		Markup: &tg.ReplyInlineMarkup{Rows: rows},
	})
	if err != nil {
		return err
	}

	wsMsg := b.constructWebSocketMessage(fileURL, file)
	b.webServer.GetWSManager().PublishMessage(u.EffectiveChat().GetID(), wsMsg)
	return nil
}

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
		if !strings.Contains(fileURL, b.config.Port) && !strings.Contains(fileURL, "localhost") && !strings.HasPrefix(fileURL, b.config.BaseURL) {
			return "/proxy?url=" + url.QueryEscape(fileURL)
		}
	}
	return fileURL
}

func (b *TelegramBot) handleListUsers(ctx *ext.Context, u *ext.Update) error {
	if admin, _ := b.userRepository.GetUserInfo(u.EffectiveUser().ID); admin == nil || !admin.IsAdmin {
		return b.sendReply(ctx, u, "Admins only.")
	}
	users, _ := b.userRepository.GetAllUsers(0, 50)
	if len(users) == 0 {
		return b.sendReply(ctx, u, "No users.")
	}
	var msg strings.Builder
	msg.WriteString("Users:\n")
	for _, user := range users {
		auth := "No"
		if user.IsAuthorized {
			auth = "Yes"
		}
		admin := ""
		if user.IsAdmin {
			admin = " (Admin)"
		}
		msg.WriteString(fmt.Sprintf("- %s %s (@%s) - Auth: %s%s\n", user.FirstName, user.LastName, user.Username, auth, admin))
	}
	return b.sendReply(ctx, u, msg.String())
}

func (b *TelegramBot) handleUserInfo(ctx *ext.Context, u *ext.Update) error {
	if admin, _ := b.userRepository.GetUserInfo(u.EffectiveUser().ID); admin == nil || !admin.IsAdmin {
		return b.sendReply(ctx, u, "Admins only.")
	}
	args := strings.Fields(u.EffectiveMessage.Text)
	if len(args) < 2 {
		return b.sendReply(ctx, u, "Usage: /userinfo <id>")
	}
	id, _ := strconv.ParseInt(args[1], 10, 64)
	user, _ := b.userRepository.GetUserInfo(id)
	if user == nil {
		return b.sendReply(ctx, u, "User not found.")
	}
	return b.sendReply(ctx, u, fmt.Sprintf("ID: %d\nName: %s %s\n@%s\nAuth: %t\nAdmin: %t", user.UserID, user.FirstName, user.LastName, user.Username, user.IsAuthorized, user.IsAdmin))
}
